// Package integration contains AIS integration tests.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */

package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/devtools/readers"
	"github.com/NVIDIA/aistore/devtools/tassert"
	"github.com/NVIDIA/aistore/devtools/tetl"
	"github.com/NVIDIA/aistore/devtools/tlog"
	"github.com/NVIDIA/aistore/devtools/tutils"
	"github.com/NVIDIA/aistore/etl"
	"github.com/NVIDIA/aistore/etl/runtime"
	"github.com/NVIDIA/go-tfdata/tfdata/core"
)

const (
	tar2tfIn  = "data/small-mnist-3.tar"
	tar2tfOut = "data/small-mnist-3.record"

	tar2tfFiltersIn  = "data/single-png-cls.tar"
	tar2tfFiltersOut = "data/single-png-cls-transformed.tfrecord"
)

type (
	filesEqualFunc func(f1, f2 string) (bool, error)

	testObjConfig struct {
		transformer string
		comm        string
		inPath      string         // optional
		outPath     string         // optional
		filesEqual  filesEqualFunc // optional
		onlyLong    bool           // run only with long tests
	}

	testCloudObjConfig struct {
		cached   bool
		onlyLong bool
	}
)

func (tc testObjConfig) Name() string {
	return fmt.Sprintf("%s/%s", tc.transformer, strings.TrimSuffix(tc.comm, "://"))
}

// TODO: This should be a part of go-tfdata.
// This function is necessary, as the same TFRecords can be different byte-wise.
// This is caused by the fact that order of TFExamples is can de different,
// as well as ordering of elements of a single TFExample can be different.
func tfDataEqual(n1, n2 string) (bool, error) {
	examples1, err := readExamples(n1)
	if err != nil {
		return false, err
	}
	examples2, err := readExamples(n2)
	if err != nil {
		return false, err
	}

	if len(examples1) != len(examples2) {
		return false, nil
	}
	return tfRecordsEqual(examples1, examples2)
}

func tfRecordsEqual(examples1, examples2 []*core.TFExample) (bool, error) {
	sort.SliceStable(examples1, func(i, j int) bool {
		return examples1[i].GetFeature("__key__").String() < examples1[j].GetFeature("__key__").String()
	})
	sort.SliceStable(examples2, func(i, j int) bool {
		return examples2[i].GetFeature("__key__").String() < examples2[j].GetFeature("__key__").String()
	})

	for i := 0; i < len(examples1); i++ {
		if !reflect.DeepEqual(examples1[i].ProtoReflect(), examples2[i].ProtoReflect()) {
			return false, nil
		}
	}
	return true, nil
}

func readExamples(fileName string) (examples []*core.TFExample, err error) {
	f, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return core.NewTFRecordReader(f).ReadAllExamples()
}

func testETLObject(t *testing.T, onlyLong bool, comm, transformer, inPath, outPath string, fEq filesEqualFunc) {
	var (
		bck                    = cmn.Bck{Provider: cmn.ProviderAIS, Name: "etl-test"}
		testObjDir             = filepath.Join("data", "transformer", transformer)
		inputFileName          = filepath.Join(testObjDir, "object.in")
		expectedOutputFileName = filepath.Join(testObjDir, "object.out")

		objName        = fmt.Sprintf("%s-%s-object", transformer, cos.RandString(5))
		outputFileName = filepath.Join(t.TempDir(), objName+".out")

		uuid string
	)

	tutils.CheckSkip(t, tutils.SkipTestArgs{Bck: bck, Long: onlyLong})

	if fEq == nil {
		fEq = tutils.FilesEqual
	}
	if inPath != "" {
		inputFileName = inPath
	}
	if outPath != "" {
		expectedOutputFileName = outPath
	}

	tlog.Logln("Creating bucket")
	tutils.CreateFreshBucket(t, proxyURL, bck, nil)
	defer tutils.DestroyBucket(t, proxyURL, bck)

	tlog.Logln("Putting object")
	reader, err := readers.NewFileReaderFromFile(inputFileName, cos.ChecksumNone)
	tassert.CheckFatal(t, err)
	tutils.PutObject(t, bck, objName, reader)

	uuid, err = tetl.Init(baseParams, transformer, comm)
	tassert.CheckFatal(t, err)
	t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid) })

	fho, err := cos.CreateFile(outputFileName)
	tassert.CheckFatal(t, err)
	defer fho.Close()

	tlog.Logf("Read %q\n", uuid)
	err = api.ETLObject(baseParams, uuid, bck, objName, fho)
	tassert.CheckFatal(t, err)

	tlog.Logln("Compare output")
	same, err := fEq(outputFileName, expectedOutputFileName)
	tassert.CheckError(t, err)
	tassert.Errorf(t, same, "file contents after transformation differ")
}

func testETLObjectCloud(t *testing.T, bck cmn.Bck, uuid string, onlyLong, cached bool) {
	// Always uses Echo transformation, as correctness of other transformations is checked in different tests.
	tutils.CheckSkip(t, tutils.SkipTestArgs{Long: onlyLong})

	objName := fmt.Sprintf("%s-%s-object", uuid, cos.RandString(5))
	tlog.Logln("Putting object")
	reader, err := readers.NewRandReader(cos.KiB, cos.ChecksumNone)
	tassert.CheckFatal(t, err)

	err = api.PutObject(api.PutObjectArgs{
		BaseParams: baseParams,
		Bck:        bck,
		Object:     objName,
		Reader:     reader,
	})
	tassert.CheckFatal(t, err)

	if !cached {
		tlog.Logf("Evicting object %s\n", objName)
		err := api.EvictObject(baseParams, bck, objName)
		tassert.CheckFatal(t, err)
	}

	defer func() {
		// Could bucket is not destroyed, remove created object instead.
		err := api.DeleteObject(baseParams, bck, objName)
		tassert.CheckError(t, err)
	}()

	bf := bytes.NewBuffer(nil)
	tlog.Logf("Read %q\n", uuid)
	err = api.ETLObject(baseParams, uuid, bck, objName, bf)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, bf.Len() == cos.KiB, "Expected %d bytes, got %d", cos.KiB, bf.Len())
}

// Responsible for cleaning ETL xaction, ETL containers, destination bucket.
func testETLBucket(t *testing.T, uuid string, bckFrom cmn.Bck, objCnt int, fileSize uint64, timeout time.Duration) {
	var (
		bckTo          = cmn.Bck{Name: "etloffline-out-" + cos.RandString(5), Provider: cmn.ProviderAIS}
		requestTimeout = 30 * time.Second
	)
	t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid) })

	tlog.Logf("Start offline ETL %q\n", uuid)
	xactID := tetl.ETLBucket(t, baseParams, bckFrom, bckTo, &cmn.TransCpyBckMsg{
		ID:             uuid,
		RequestTimeout: cos.Duration(requestTimeout),
	})

	err := tetl.WaitForFinished(baseParams, xactID, timeout)
	tassert.CheckFatal(t, err)

	list, err := api.ListObjects(baseParams, bckTo, nil, 0)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, len(list.Entries) == objCnt, "expected %d objects from offline ETL, got %d", objCnt, len(list.Entries))
	checkETLStats(t, xactID, objCnt, fileSize*uint64(objCnt))
}

// NOTE: BytesCount references number of bytes *before* the transformation.
func checkETLStats(t *testing.T, xactID string, expectedObjCnt int, expectedBytesCnt uint64) {
	stats, err := api.GetXactionStatsByID(baseParams, xactID)
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, stats.ObjCount() == int64(expectedObjCnt), "stats expected to return %d objects, got %d", expectedObjCnt, stats.ObjCount())
	// If expectedBytesCnt == 0 don't check it as we don't now the precise size.
	tassert.Errorf(t, expectedBytesCnt == 0 || uint64(stats.BytesCount()) == expectedBytesCnt, "stats expected to return %d bytes, got %d", expectedBytesCnt, stats.BytesCount())
}

func TestETLObject(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	tests := []testObjConfig{
		{transformer: tetl.Echo, comm: etl.RedirectCommType, onlyLong: true},
		{transformer: tetl.Echo, comm: etl.RevProxyCommType, onlyLong: true},
		{transformer: tetl.Echo, comm: etl.PushCommType, onlyLong: true},
		{tetl.Tar2TF, etl.RedirectCommType, tar2tfIn, tar2tfOut, tfDataEqual, true},
		{tetl.Tar2TF, etl.RevProxyCommType, tar2tfIn, tar2tfOut, tfDataEqual, true},
		{tetl.Tar2TF, etl.PushCommType, tar2tfIn, tar2tfOut, tfDataEqual, true},
		{tetl.Tar2tfFilters, etl.RedirectCommType, tar2tfFiltersIn, tar2tfFiltersOut, tfDataEqual, false},
		{tetl.Tar2tfFilters, etl.RevProxyCommType, tar2tfFiltersIn, tar2tfFiltersOut, tfDataEqual, false},
		{tetl.Tar2tfFilters, etl.PushCommType, tar2tfFiltersIn, tar2tfFiltersOut, tfDataEqual, false},
	}

	for _, test := range tests {
		t.Run(test.Name(), func(t *testing.T) {
			testETLObject(t, test.onlyLong, test.comm, test.transformer, test.inPath, test.outPath, test.filesEqual)
		})
	}
}

func TestETLObjectCloud(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{Bck: cliBck, RequiredDeployment: tutils.ClusterTypeK8s, RemoteBck: true})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	// TODO: When a test is stable, make part of test cases onlyLong: true.
	tcs := map[string][]*testCloudObjConfig{
		etl.RedirectCommType: {
			{cached: true, onlyLong: false},
			{cached: false, onlyLong: false},
		},
		etl.RevProxyCommType: {
			{cached: true, onlyLong: false},
			{cached: false, onlyLong: false},
		},
		etl.PushCommType: {
			{cached: true, onlyLong: false},
			{cached: false, onlyLong: false},
		},
	}

	for comm, configs := range tcs {
		t.Run(comm, func(t *testing.T) {
			uuid, err := tetl.Init(baseParams, tetl.Echo, comm)
			tassert.CheckFatal(t, err)
			t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid) })

			for _, conf := range configs {
				t.Run(fmt.Sprintf("cached=%t", conf.cached), func(t *testing.T) {
					testETLObjectCloud(t, cliBck, uuid, conf.onlyLong, conf.cached)
				})
			}
		})
	}
}

func TestETLBucket(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	var (
		bck    = cmn.Bck{Name: "etloffline", Provider: cmn.ProviderAIS}
		objCnt = 10

		m = ioContext{
			t:         t,
			num:       objCnt,
			fileSize:  512,
			fixedSize: true,
			bck:       bck,
		}

		tests = []testObjConfig{
			{transformer: tetl.Echo, comm: etl.RedirectCommType, onlyLong: true},
			{transformer: tetl.Md5, comm: etl.RevProxyCommType},
			{transformer: tetl.Md5, comm: etl.PushCommType, onlyLong: true},
		}
	)

	tlog.Logln("Preparing source bucket")
	tutils.CreateFreshBucket(t, proxyURL, bck, nil)
	m.init()

	tlog.Logln("Putting objects to source bucket")
	m.puts()

	for _, test := range tests {
		t.Run(test.Name(), func(t *testing.T) {
			tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: test.onlyLong})

			uuid, err := tetl.Init(baseParams, test.transformer, test.comm)
			tassert.CheckFatal(t, err)

			testETLBucket(t, uuid, bck, objCnt, m.fileSize, time.Minute)
		})
	}
}

func TestETLBuild(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	const (
		md5 = `
import hashlib

def transform(input_bytes):
    md5 = hashlib.md5()
    md5.update(input_bytes)
    return md5.hexdigest().encode()
`

		numpy = `
import numpy as np

def transform(input_bytes: bytes) -> bytes:
    x = np.array([[0, 1], [2, 3]], dtype='<u2')
    return x.tobytes()
`
		numpyDeps = `numpy==1.19.2`
	)

	var (
		m = ioContext{
			t:         t,
			num:       10,
			fileSize:  512,
			fixedSize: true,
			bck:       cmn.Bck{Name: "etl_build", Provider: cmn.ProviderAIS},
		}

		tests = []struct {
			name     string
			code     string
			deps     string
			runtime  string
			onlyLong bool
		}{
			{name: "simple_python2", code: md5, deps: "", runtime: runtime.Python2, onlyLong: false},
			{name: "simple_python3", code: md5, deps: "", runtime: runtime.Python3, onlyLong: false},
			{name: "with_deps_python3", code: numpy, deps: numpyDeps, runtime: runtime.Python3, onlyLong: true},
		}
	)

	tlog.Logln("Preparing source bucket")
	tutils.CreateFreshBucket(t, proxyURL, m.bck, nil)

	m.init()

	tlog.Logln("Putting objects to source bucket")
	m.puts()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: test.onlyLong})

			uuid, err := api.ETLBuild(baseParams, etl.BuildMsg{
				Code:        []byte(test.code),
				Deps:        []byte(test.deps),
				Runtime:     test.runtime,
				WaitTimeout: cos.Duration(5 * time.Minute),
			})
			tassert.CheckFatal(t, err)

			testETLBucket(t, uuid, m.bck, m.num, m.fileSize, time.Minute)
		})
	}
}

func TestETLBucketDryRun(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	var (
		bckFrom = cmn.Bck{Name: "etloffline", Provider: cmn.ProviderAIS}
		bckTo   = cmn.Bck{Name: "etloffline-out-" + cos.RandString(5), Provider: cmn.ProviderAIS}
		objCnt  = 10

		m = ioContext{
			t:         t,
			num:       objCnt,
			fileSize:  512,
			fixedSize: true,
			bck:       bckFrom,
		}
	)

	tlog.Logln("Preparing source bucket")
	tutils.CreateFreshBucket(t, proxyURL, bckFrom, nil)
	m.init()

	tlog.Logln("Putting objects to source bucket")
	m.puts()

	uuid, err := tetl.Init(baseParams, tetl.Echo, etl.RevProxyCommType)
	tassert.CheckFatal(t, err)
	t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid) })

	tlog.Logf("Start offline ETL %q\n", uuid)
	xactID, err := api.ETLBucket(baseParams, bckFrom, bckTo,
		&cmn.TransCpyBckMsg{ID: uuid, CopyBckMsg: cmn.CopyBckMsg{DryRun: true}})
	tassert.CheckFatal(t, err)

	args := api.XactReqArgs{ID: xactID, Timeout: time.Minute}
	_, err = api.WaitForXaction(baseParams, args)
	tassert.CheckFatal(t, err)

	exists, err := api.DoesBucketExist(baseParams, cmn.QueryBcks(bckTo))
	tassert.CheckFatal(t, err)
	tassert.Errorf(t, exists == false, "expected destination bucket to not be created")

	checkETLStats(t, xactID, m.num, uint64(m.num*int(m.fileSize)))
}

func TestETLSingleTransformerAtATime(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: true})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	output, err := exec.Command("bash", "-c", "kubectl get nodes | grep Ready | wc -l").CombinedOutput()
	tassert.CheckFatal(t, err)
	if strings.Trim(string(output), "\n") != "1" {
		t.Skip("Requires a single node kubernetes cluster")
	}

	if tutils.GetClusterMap(t, proxyURL).CountTargets() > 1 {
		t.Skip("Requires a single-node single-target deployment")
	}

	uuid1, err := tetl.Init(baseParams, tetl.Echo, etl.RevProxyCommType)
	tassert.CheckFatal(t, err)
	t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid1) })

	uuid2, err := tetl.Init(baseParams, tetl.Md5, etl.RevProxyCommType)
	tassert.Errorf(t, err != nil, "expected err to occur")
	if uuid2 != "" {
		tetl.StopETL(t, baseParams, uuid2)
	}
}

func TestETLHealth(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s, Long: true})
	tetl.CheckNoRunningETLContainers(t, baseParams)

	tlog.Logln("Starting ETL")
	uuid, err := tetl.Init(baseParams, tetl.Echo, etl.RedirectCommType)
	tassert.CheckFatal(t, err)
	t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid) })

	var (
		start    = time.Now()
		deadline = start.Add(2 * time.Minute)
		healths  etl.PodsHealthMsg
	)

	// It might take some time for metrics to be available.
	for {
		now := time.Now()
		if now.After(deadline) {
			t.Fatal("Timeout waiting for successful health response")
		}

		healths, err = api.ETLHealth(baseParams, uuid)
		if err == nil {
			if len(healths) > 0 {
				tlog.Logf("Successfully received metrics after %s\n", now.Sub(start))
				break
			}
			tlog.Logln("Unexpected empty health messages without error, retrying...")
			continue
		}

		httpErr, ok := err.(*cmn.ErrHTTP)
		tassert.Fatalf(t, ok && httpErr.Status == http.StatusNotFound, "Unexpected error %v, expected 404", err)
		tlog.Logf("ETL %q not found in metrics, retrying...\n", uuid)
		time.Sleep(10 * time.Second)
	}

	for _, health := range healths {
		tassert.Errorf(t, health.CPU > 0.0 || health.Mem > 0, "[%s] expected non empty health info, got %v", health.TargetID, health)
	}
}

func TestETLList(t *testing.T) {
	tutils.CheckSkip(t, tutils.SkipTestArgs{RequiredDeployment: tutils.ClusterTypeK8s})

	uuid, err := tetl.Init(baseParams, tetl.Echo, etl.RevProxyCommType)
	tassert.CheckFatal(t, err)
	t.Cleanup(func() { tetl.StopETL(t, baseParams, uuid) })

	list, err := api.ETLList(baseParams)
	tassert.CheckFatal(t, err)
	tassert.Fatalf(t, len(list) == 1, "expected exactly one ETL to be listed, got %d", len(list))
	tassert.Fatalf(t, list[0].ID == uuid, "expected uuid to be %q, got %q", uuid, list[0].ID)
}
