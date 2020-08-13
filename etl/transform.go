// Package etl provides utilities to initialize and use transformation pods.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package etl

import (
	"bytes"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// built-in label https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#built-in-node-labels
	nodeNameLabel = "kubernetes.io/hostname"
	targetNode    = "target_node"

	tfProbeRetries = 5
)

var (
	tfProbeClient = &http.Client{}
)

// Definitions:
//
// ETL -> This refers to Extract-Transform-Load, which allows a user to do transformation
//        of the objects. It is defined by an ETL spec which is a K8S yaml spec file.
//        The operations of an ETL are executed on the ETL container.
//
// ETL container -> The user's K8S pod which runs the container doing the transformation of the
//                  objects.  It is initiated by a target and runs on the same K8S node running
//                  the target.
// Flow:
// 1. User initiates an ETL container, using the `Start` method.
// 2. The ETL container starts on the same node as the target.
// 3. The transformation is done using `Communicator.Do()`
// 4. The ETL container is stopped using `Stop`, which deletes the K8S pod.
//
// Limitations of the current implementation (soon to be removed):
//
// * No idle timeout for a ETL container. It keeps running unless explicitly
//   stopped by invoking the `Stop` API.
//
// * `kubectl delete` of a ETL container is done in two stages. First we gracefully try to terminate
//   the pod with a 30s timeout. Upon failure to do so, we perform a force delete.
//
// * A single ETL container runs per target at any point of time.
//
// * Recreating a ETL container with the same name, will delete any containers running with
//   the same name.
//
// * TODO: replace `kubectl` calls with proper go-sdk calls.

type (
	Aborter struct {
		t           cluster.Target
		currentSmap *cluster.Smap
		uuid        string
		mtx         sync.Mutex
	}
)

func NewAborter(t cluster.Target, uuid string) *Aborter {
	return &Aborter{
		uuid:        uuid,
		t:           t,
		currentSmap: t.GetSowner().Get(),
	}
}

func (e *Aborter) String() string {
	return fmt.Sprintf("etl-aborter-%s", e.uuid)
}

func (e *Aborter) ListenSmapChanged() {
	// New goroutine as kubectl calls can take a lot of time,
	// making other listeners wait.
	go func() {
		e.mtx.Lock()
		defer e.mtx.Unlock()
		newSmap := e.t.GetSowner().Get()

		if newSmap.Version <= e.currentSmap.Version {
			return
		}

		if !newSmap.CompareTargets(e.currentSmap) {
			glog.Errorf("[ETL-UUID=%s] targets have changed, aborting...", e.uuid)
			// Stop will unregister e from smap listeners.
			if err := Stop(e.t, e.uuid); err != nil {
				glog.Error(err.Error())
			}
		}

		e.currentSmap = newSmap
	}()
}

// TODO: remove the `kubectl` with a proper go-sdk call
func Start(t cluster.Target, msg Msg) (err error) {
	var (
		pod             *corev1.Pod
		svc             *corev1.Service
		hostIP          string
		originalPodName string

		errCtx = &cmn.ETLErrorContext{
			Tid:  t.Snode().DaemonID,
			UUID: msg.ID,
		}
	)
	cmn.Assert(t.K8sNodeName() != "") // Corresponding 'if' done at the beginning of the request.
	// Parse spec template.
	if pod, err = ParsePodSpec(errCtx, msg.Spec); err != nil {
		return err
	}
	errCtx.ETLName = pod.GetName()
	originalPodName = pod.GetName()

	// Override the name (add target's daemon ID and node ID to its name).
	pod.SetName(pod.GetName() + "-" + t.Snode().DaemonID + "-" + t.K8sNodeName())
	errCtx.PodName = pod.GetName()
	if pod.Labels == nil {
		pod.Labels = make(map[string]string, 1)
	}
	pod.Labels[targetNode] = t.K8sNodeName()

	// Create service spec
	svc = createServiceSpec(pod)
	errCtx.SvcName = svc.Name

	// The following combination of Affinity and Anti-Affinity allows one to
	// achieve the following:
	// 1. The ETL container is always scheduled on the target invoking it.
	// 2. Not more than one ETL container with the same target, is scheduled on the same node,
	//    at a given point of time
	if err := setTransformAffinity(errCtx, t, pod); err != nil {
		return err
	}

	if err := setTransformAntiAffinity(errCtx, t, pod); err != nil {
		return err
	}

	setPodEnvVariables(pod, t)

	// 1. Doing cleanup of any pre-existing entities
	if err := deleteEntity(errCtx, cmn.KubePod, pod.Name); err != nil {
		return err
	}

	if err := deleteEntity(errCtx, cmn.KubeSvc, svc.Name); err != nil {
		return err
	}

	// 2. Creating pod
	if err := createEntity(errCtx, cmn.KubePod, pod); err != nil {
		// Ignoring the error for deletion as it is best effort.
		glog.Errorf("Failed creation of pod %q. Doing cleanup.", pod.Name)
		if deleteErr := deleteEntity(errCtx, cmn.KubePod, pod.Name); deleteErr != nil {
			glog.Errorf("%s: %s", deleteErr.Error(), "Failed to delete pod after it's failed starting")
		}
		return err
	}

	if err := waitPodReady(errCtx, pod, msg.WaitTimeout); err != nil {
		return err
	}

	// Retrieve host IP of the pod.
	if hostIP, err = getPodHostIP(errCtx, pod); err != nil {
		return err
	}

	// 3. Creating service
	if err := createEntity(errCtx, cmn.KubeSvc, svc); err != nil {
		// Ignoring the error for deletion as it is best effort.
		glog.Errorf("Failed creation of svc %q. Doing cleanup.", svc.Name)
		if deleteErr := deleteEntity(errCtx, cmn.KubeSvc, svc.Name); deleteErr != nil {
			glog.Errorf("%s: %s", deleteErr.Error(), "Failed to delete service after it's failed starting")
		}
		if deleteErr := deleteEntity(errCtx, cmn.KubePod, pod.Name); deleteErr != nil {
			glog.Errorf("%s: %s", deleteErr.Error(), "Failed to delete pod after it's corresponding service failed starting")
		}
		return err
	}

	nodePort, err := getServiceNodePort(errCtx, svc)
	if err != nil {
		return err
	}

	transformerURL := fmt.Sprintf("http://%s:%d", hostIP, nodePort)

	// TODO: Temporary workaround. Debug this further to find the root cause.
	// (not waiting sometimes causes the first Do() to fail)
	readinessPath := pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path
	if waitErr := waitTransformerReady(transformerURL, readinessPath); waitErr != nil {
		if err := deleteEntity(errCtx, cmn.KubePod, pod.Name); err != nil {
			glog.Error(err)
		}
		if err := deleteEntity(errCtx, cmn.KubeSvc, svc.Name); err != nil {
			glog.Error(err)
		}
		return cmn.NewETLError(errCtx, waitErr.Error())
	}

	c := makeCommunicator(t, pod, msg.CommType, transformerURL, originalPodName, NewAborter(t, msg.ID))
	if err := reg.put(msg.ID, c); err != nil {
		return err
	}
	t.GetSowner().Listeners().Reg(c)
	return nil
}

func waitTransformerReady(url, path string) (err error) {
	var resp *http.Response
	tfProbeSleep := cmn.GCO.Get().Timeout.MaxKeepalive
	tfProbeClient.Timeout = tfProbeSleep
	for i := 0; i < tfProbeRetries; i++ {
		resp, err = tfProbeClient.Get(cmn.JoinPath(url, path))
		if err != nil {
			glog.Errorf("failing to GET %s, err: %v, cnt: %d", cmn.JoinPath(url, path), err, i+1)
			if cmn.IsReqCanceled(err) || cmn.IsErrConnectionRefused(err) {
				time.Sleep(tfProbeSleep)
				continue
			}
			return
		}
		err = cmn.DrainReader(resp.Body)
		resp.Body.Close()
		break
	}
	return
}

func createServiceSpec(pod *corev1.Pod) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: pod.GetName(),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Port: pod.Spec.Containers[0].Ports[0].ContainerPort},
			},
			Selector: map[string]string{
				"app": pod.Labels["app"],
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}
}

// Stop deletes all occupied by the ETL resources, including Pods and Services.
// It unregisters ETL smap listener.
func Stop(t cluster.Target, id string) error {
	var (
		errCtx = &cmn.ETLErrorContext{
			UUID: id,
			Tid:  t.Snode().DaemonID,
		}
	)

	c, err := GetCommunicator(id)
	if err != nil {
		return cmn.NewETLError(errCtx, err.Error())
	}
	errCtx.PodName = c.PodName()
	errCtx.SvcName = c.SvcName()

	if err := deleteEntity(errCtx, cmn.KubePod, c.PodName()); err != nil {
		return err
	}

	if err := deleteEntity(errCtx, cmn.KubeSvc, c.SvcName()); err != nil {
		return err
	}

	if c := reg.removeByUUID(id); c != nil {
		t.GetSowner().Listeners().Unreg(c)
	}

	return nil
}

func GetCommunicator(transformID string) (Communicator, error) {
	c, exists := reg.getByUUID(transformID)
	if !exists {
		return nil, cmn.NewNotFoundError("transformation with %q id", transformID)
	}
	return c, nil
}

func List() []Info { return reg.list() }

// Sets pods node affinity, so pod will be scheduled on the same node as a target creating it.
func setTransformAffinity(errCtx *cmn.ETLErrorContext, t cluster.Target, pod *corev1.Pod) error {
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}

	reqAffinity := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	prefAffinity := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution

	if reqAffinity != nil && len(reqAffinity.NodeSelectorTerms) > 0 || len(prefAffinity) > 0 {
		return cmn.NewETLError(errCtx, "error in YAML spec, pod should not have any NodeAffinities defined")
	}

	nodeSelector := &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key:      nodeNameLabel,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{t.K8sNodeName()},
			}}},
		},
	}
	// TODO: RequiredDuringSchedulingIgnoredDuringExecution means that ETL container
	//  will be placed on the same machine as target which creates it. However,
	//  if 2 targets went down and up again at the same time, they may switch nodes,
	//  leaving ETL containers running on the wrong machines.
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nodeSelector
	return nil
}

// Sets pods node anti-affinity, so no two pods with the matching criteria is scheduled on the same node
// at the same time.
func setTransformAntiAffinity(errCtx *cmn.ETLErrorContext, t cluster.Target, pod *corev1.Pod) error {
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.PodAntiAffinity == nil {
		pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}

	reqAntiAffinities := pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	prefAntiAffinity := pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution

	if len(reqAntiAffinities) > 0 || len(prefAntiAffinity) > 0 {
		return cmn.NewETLError(errCtx, "error in YAML spec, pod should not have any NodeAntiAffinities defined")
	}

	reqAntiAffinities = []corev1.PodAffinityTerm{{
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				targetNode: t.K8sNodeName(),
			},
		},
		TopologyKey: nodeNameLabel,
	}}
	pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = reqAntiAffinities
	return nil
}

// Sets environment variables that can be accessed inside the container.
func setPodEnvVariables(pod *corev1.Pod, t cluster.Target) {
	containers := pod.Spec.Containers
	for idx := range containers {
		containers[idx].Env = append(containers[idx].Env, corev1.EnvVar{
			Name:  "AIS_TARGET_URL",
			Value: t.Snode().URL(cmn.NetworkPublic),
		})
	}
}

func waitPodReady(errCtx *cmn.ETLErrorContext, pod *corev1.Pod, waitTimeout cmn.DurationJSON) error {
	args := []string{"wait"}
	if !waitTimeout.IsZero() {
		args = append(args, "--timeout", waitTimeout.String())
	}
	args = append(args, "--for", "condition=ready", "pod", pod.GetName())
	cmd := exec.Command(cmn.Kubectl, args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		handlePodFailure(errCtx, pod, "pod start failure")
		return cmn.NewETLError(errCtx, "failed waiting for pod to get ready (err: %v; out: %s)", err, string(b))
	}
	return nil
}

func getPodHostIP(errCtx *cmn.ETLErrorContext, pod *corev1.Pod) (string, error) {
	// Retrieve host IP of the pod.
	output, err := exec.Command(cmn.Kubectl, []string{"get", "pod", pod.GetName(), "--template={{.status.hostIP}}"}...).CombinedOutput()
	if err != nil {
		return "", cmn.NewETLError(errCtx, "failed to get IP of pod (err: %v; output: %s)", err, string(output))
	}
	return string(output), nil
}

func deleteEntity(errCtx *cmn.ETLErrorContext, entity, entityName string) error {
	var (
		args = []string{"delete", entity, entityName, "--ignore-not-found"}
	)

	// Doing graceful delete
	output, err := exec.Command(cmn.Kubectl, args...).CombinedOutput()
	if err == nil {
		return nil
	}

	etlErr := cmn.NewETLError(errCtx, "failed to delete %s, err: %v, out: %s. Retrying with --force", entity, err, string(output))
	glog.Errorf(etlErr.Error())

	// Doing force delete
	args = append(args, "--force")
	output, err = exec.Command(cmn.Kubectl, args...).CombinedOutput()
	if err != nil {
		return cmn.NewETLError(errCtx, "force delete failed. %q %s, err: %v, out: %s",
			entity, entityName, err, string(output))
	}
	return nil
}

func createEntity(errCtx *cmn.ETLErrorContext, entity string, spec interface{}) error {
	var (
		b    = cmn.MustMarshal(spec)
		args = []string{"create", "-f", "-"}
		cmd  = exec.Command(cmn.Kubectl, args...)
	)

	cmd.Stdin = bytes.NewBuffer(b)
	if b, err := cmd.CombinedOutput(); err != nil {
		return cmn.NewETLError(errCtx, "failed to create %s (err: %v; output: %s)", entity, err, string(b))
	}
	return nil
}

func getServiceNodePort(errCtx *cmn.ETLErrorContext, svc *corev1.Service) (int, error) {
	output, err := exec.Command(cmn.Kubectl, []string{"get", "-o", "jsonpath=\"{.spec.ports[0].nodePort}\"", "svc", svc.GetName()}...).CombinedOutput()
	if err != nil {
		return -1, cmn.NewETLError(errCtx, "failed to get nodePort for service %q (err: %v; output: %s)", svc.GetName(), err, string(output))
	}
	outputStr, _ := strconv.Unquote(string(output))
	nodePort, err := strconv.Atoi(outputStr)
	if err != nil {
		return -1, cmn.NewETLError(errCtx, "failed to parse nodePort for pod-svc %q (err: %v; output: %s)", svc.GetName(), err, string(output))
	}
	return nodePort, nil
}

func handlePodFailure(errCtx *cmn.ETLErrorContext, pod *corev1.Pod, msg string) {
	if deleteErr := deleteEntity(errCtx, cmn.KubePod, pod.GetName()); deleteErr != nil {
		glog.Errorf("%s: %s", deleteErr.Error(), "failed to delete pod after "+msg)
	}
}