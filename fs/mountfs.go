// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/ios"
	"github.com/OneOfOne/xxhash"
)

const (
	TrashDir      = "$trash"
	daemonIDXattr = "user.ais.daemon_id"
)

const (
	siMpathIDMismatch = (1 + iota) * 10
	siTargetIDMismatch
	siMetaMismatch
	siMetaCorrupted
	siMpathMissing
)

// Terminology:
// - a mountpath is equivalent to (configurable) fspath - both terms are used interchangeably;
// - each mountpath is, simply, a local directory that is serviced by a local filesystem;
// - there's a 1-to-1 relationship between a mountpath and a local filesystem
//   (different mountpaths map onto different filesystems, and vise versa);
// - mountpaths of the form <filesystem-mountpoint>/a/b/c are supported.

type (
	MountpathInfo struct {
		Path           string   // cleaned up
		FilesystemInfo          // name of the underlying filesystem, its ID and other info
		PathDigest     uint64   // used for HRW
		Disks          []string // owned disks (ios.FsDisks map => slice)

		// LOM caches
		lomCaches cos.MultiSyncMap
		// bucket path cache
		bpc struct {
			sync.RWMutex
			m map[uint64]string
		}
		// capacity
		cmu      sync.RWMutex
		capacity Capacity
		// String
		info string
	}
	MPI map[string]*MountpathInfo

	Capacity struct {
		Used    uint64 `json:"used,string"`  // bytes
		Avail   uint64 `json:"avail,string"` // ditto
		PctUsed int32  `json:"pct_used"`     // %% used (redundant ok)
	}
	MPCap map[string]Capacity // [mpath => Capacity]

	// MountedFS holds all mountpaths for the target.
	MountedFS struct {
		mu sync.RWMutex
		// fsIDs is set in which we store fsids of mountpaths. This allows for
		// determining if there are any duplications of file system - we allow
		// only one mountpath per file system.
		fsIDs map[cos.FsID]string
		// checkFsID determines if we should actually check FSID when adding new
		// mountpath. By default it is set to true.
		checkFsID bool
		// Available mountpaths - mountpaths which are used to store the data.
		available atomic.Pointer
		// Disabled mountpaths - mountpaths which for some reason did not pass
		// the health check and cannot be used for a moment.
		disabled atomic.Pointer
		// Iostats for the available mountpaths
		ios ios.IOStater

		// capacity
		cmu     sync.RWMutex
		capTime struct {
			curr, next int64
		}
		capStatus CapStatus
	}
	CapStatus struct {
		TotalUsed  uint64 // bytes
		TotalAvail uint64 // bytes
		PctAvg     int32  // used average (%)
		PctMax     int32  // max used (%)
		Err        error
		OOS        bool
	}
	ErrMpathNoDisks struct {
		mi *MountpathInfo
	}
)

var (
	mfs *MountedFS

	ErrNoMountpaths = errors.New("no mountpaths")
)

func (e *ErrMpathNoDisks) Error() string { return fmt.Sprintf("%s has no disks", e.mi) }

///////////////////
// MountpathInfo //
///////////////////

func newMountpath(mpath, tid string) (mi *MountpathInfo, err error) {
	var (
		cleanMpath string
		fsInfo     FilesystemInfo
	)
	if cleanMpath, err = cmn.ValidateMpath(mpath); err != nil {
		return
	}
	if err = Access(cleanMpath); err != nil {
		return nil, cmn.NewNotFoundError("t[%s]: mountpath %q", tid, mpath)
	}
	if fsInfo, err = makeFsInfo(cleanMpath); err != nil {
		return
	}
	mi = &MountpathInfo{
		Path:           cleanMpath,
		FilesystemInfo: fsInfo,
		PathDigest:     xxhash.ChecksumString64S(cleanMpath, cos.MLCG32),
	}
	mi.bpc.m = make(map[uint64]string, 16)
	return
}

func (mi *MountpathInfo) String() string { return mi._string() }

func (mi *MountpathInfo) _string() string {
	if mi.info != "" {
		return mi.info
	}
	switch len(mi.Disks) {
	case 0:
		mi.info = fmt.Sprintf("mp[%s, fs=%s]", mi.Path, mi.Fs)
	case 1:
		mi.info = fmt.Sprintf("mp[%s, %s]", mi.Path, mi.Disks[0])
	default:
		mi.info = fmt.Sprintf("mp[%s, %v]", mi.Path, mi.Disks)
	}
	return mi.info
}

func (mi *MountpathInfo) LomCache(idx int) *sync.Map { return mi.lomCaches.Get(idx) }

func (mi *MountpathInfo) EvictLomCache() {
	for idx := 0; idx < cos.MultiSyncMapCount; idx++ {
		cache := mi.LomCache(idx)
		cache.Range(func(key interface{}, _ interface{}) bool {
			cache.Delete(key)
			return true
		})
	}
}

func (mi *MountpathInfo) MakePathTrash() string { return filepath.Join(mi.Path, TrashDir) }

// MoveToTrash removes directory in steps:
// 1. Synchronously gets temporary directory name
// 2. Synchronously renames old folder to temporary directory
func (mi *MountpathInfo) MoveToTrash(dir string) error {
	// Loose assumption: removing something which doesn't exist is fine.
	if err := Access(dir); err != nil && os.IsNotExist(err) {
		return nil
	}
Retry:
	var (
		trashDir = mi.MakePathTrash()
		tmpDir   = filepath.Join(trashDir, fmt.Sprintf("$dir-%d", mono.NanoTime()))
	)
	if err := cos.CreateDir(trashDir); err != nil {
		return err
	}
	if err := os.Rename(dir, tmpDir); err != nil {
		if os.IsExist(err) {
			// Slow path: `tmpDir` already exists so let's retry. It should
			// never happen but who knows...
			glog.Warningf("directory %q already exist in trash", tmpDir)
			goto Retry
		}
		if os.IsNotExist(err) {
			// Someone removed `dir` before `os.Rename`, nothing more to do.
			return nil
		}
		return err
	}
	// TODO: remove and make it work when the space is extremely constrained (J)
	debug.Func(func() {
		go func() {
			if err := os.RemoveAll(tmpDir); err != nil {
				glog.Errorf("RemoveAll for %q failed with %v", tmpDir, err)
			}
		}()
	})
	return nil
}

func (mi *MountpathInfo) IsIdle(config *cmn.Config) bool {
	curr := mfs.ios.GetMpathUtil(mi.Path)
	return curr >= 0 && curr < config.Disk.DiskUtilLowWM
}

func (mi *MountpathInfo) CreateMissingBckDirs(bck cmn.Bck) (err error) {
	for contentType := range CSM.RegisteredContentTypes {
		dir := mi.MakePathCT(bck, contentType)
		if err = Access(dir); err == nil {
			continue
		}
		if err = cos.CreateDir(dir); err != nil {
			return
		}
	}
	return
}

func (mi *MountpathInfo) backupAtmost(from, backup string, bcnt, atMost int) (newBcnt int) {
	var (
		fromPath   = filepath.Join(mi.Path, from)
		backupPath = filepath.Join(mi.Path, backup)
	)
	os.Remove(backupPath)
	newBcnt = bcnt
	if bcnt >= atMost {
		return
	}
	if Access(fromPath) != nil {
		return
	}
	if err := os.Rename(fromPath, backupPath); err != nil {
		glog.Error(err)
		os.Remove(fromPath)
	} else {
		newBcnt = bcnt + 1
	}
	return
}

func (mi *MountpathInfo) ClearMDs() {
	for _, mdPath := range mdFilesDirs {
		mi.Remove(mdPath)
	}
}

func (mi *MountpathInfo) Remove(path string) error {
	fpath := filepath.Join(mi.Path, path)
	if err := os.RemoveAll(fpath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (mi *MountpathInfo) SetDaemonIDXattr(tid string) error {
	cos.Assert(tid != "")
	// Validate if mountpath already has daemon ID set.
	mpathDaeID, err := loadDaemonIDXattr(mi.Path)
	if err != nil {
		return err
	}
	if mpathDaeID == tid {
		return nil
	}
	if mpathDaeID != "" && mpathDaeID != tid {
		return newMpathIDMismatchErr(tid, mpathDaeID, mi.Path)
	}
	return SetXattr(mi.Path, daemonIDXattr, []byte(tid))
}

// make-path methods

func (mi *MountpathInfo) makePathBuf(bck cmn.Bck, contentType string, extra int) (buf []byte) {
	var provLen, nsLen, bckNameLen, ctLen int
	if contentType != "" {
		debug.Assert(len(contentType) == contentTypeLen)
		ctLen = 1 + 1 + contentTypeLen
		if bck.Props != nil && bck.Props.BID != 0 {
			mi.bpc.RLock()
			bdir, ok := mi.bpc.m[bck.Props.BID]
			mi.bpc.RUnlock()
			if ok {
				buf = make([]byte, 0, len(bdir)+ctLen+extra)
				buf = append(buf, bdir...)
				goto ct
			}
		}
	}
	if !bck.Ns.IsGlobal() {
		nsLen = 1
		if bck.Ns.IsRemote() {
			nsLen += 1 + len(bck.Ns.UUID)
		}
		nsLen += 1 + len(bck.Ns.Name)
	}
	if bck.Name != "" {
		bckNameLen = 1 + len(bck.Name)
	}
	provLen = 1 + 1 + len(bck.Provider)
	buf = make([]byte, 0, len(mi.Path)+provLen+nsLen+bckNameLen+ctLen+extra)
	buf = append(buf, mi.Path...)
	buf = append(buf, filepath.Separator, prefProvider)
	buf = append(buf, bck.Provider...)
	if nsLen > 0 {
		buf = append(buf, filepath.Separator)
		if bck.Ns.IsRemote() {
			buf = append(buf, prefNsUUID)
			buf = append(buf, bck.Ns.UUID...)
		}
		buf = append(buf, prefNsName)
		buf = append(buf, bck.Ns.Name...)
	}
	if bckNameLen > 0 {
		buf = append(buf, filepath.Separator)
		buf = append(buf, bck.Name...)
	}
ct:
	if ctLen > 0 {
		buf = append(buf, filepath.Separator, prefCT)
		buf = append(buf, contentType...)
	}
	return
}

func (mi *MountpathInfo) MakePathBck(bck cmn.Bck) string {
	if bck.Props == nil {
		buf := mi.makePathBuf(bck, "", 0)
		return *(*string)(unsafe.Pointer(&buf))
	}

	bid := bck.Props.BID
	debug.Assert(bid != 0)
	mi.bpc.RLock()
	dir, ok := mi.bpc.m[bid]
	mi.bpc.RUnlock()
	if ok {
		return dir
	}
	buf := mi.makePathBuf(bck, "", 0)
	dir = *(*string)(unsafe.Pointer(&buf))

	mi.bpc.Lock()
	mi.bpc.m[bid] = dir
	mi.bpc.Unlock()
	return dir
}

func (mi *MountpathInfo) MakePathCT(bck cmn.Bck, contentType string) string {
	debug.AssertFunc(bck.Valid, bck)
	debug.Assert(contentType != "")
	buf := mi.makePathBuf(bck, contentType, 0)
	return *(*string)(unsafe.Pointer(&buf))
}

func (mi *MountpathInfo) MakePathFQN(bck cmn.Bck, contentType, objName string) string {
	debug.AssertFunc(bck.Valid, bck)
	debug.Assert(contentType != "" && objName != "")
	buf := mi.makePathBuf(bck, contentType, 1+len(objName))
	buf = append(buf, filepath.Separator)
	buf = append(buf, objName...)
	return *(*string)(unsafe.Pointer(&buf))
}

func (mi *MountpathInfo) makeDelPathBck(bck cmn.Bck, bid uint64) string {
	mi.bpc.Lock()
	dir, ok := mi.bpc.m[bid]
	if ok {
		delete(mi.bpc.m, bid)
	}
	mi.bpc.Unlock()
	if !ok {
		dir = mi.MakePathBck(bck)
	}
	return dir
}

// Creates all CT directories for a given (mountpath, bck) - NOTE handling of empty dirs
func (mi *MountpathInfo) createBckDirs(bck cmn.Bck, nilbmd bool) (num int, err error) {
	for contentType := range CSM.RegisteredContentTypes {
		dir := mi.MakePathCT(bck, contentType)
		if err := Access(dir); err == nil {
			if nilbmd {
				// NOTE: e.g., has been decommissioned without proper cleanup, and rejoined
				glog.Errorf("bucket %s: directory %s already exists but local BMD is nil - skipping...",
					bck, dir)
				num++
				continue
			}
			names, empty, errEmpty := IsDirEmpty(dir)
			if errEmpty != nil {
				return num, errEmpty
			}
			if !empty {
				err = fmt.Errorf("bucket %s: directory %s already exists and is not empty (%v...)",
					bck, dir, names)
				if contentType != WorkfileType {
					return num, err
				}
				glog.Error(err)
			}
		} else if err := cos.CreateDir(dir); err != nil {
			return num, fmt.Errorf("bucket %s: failed to create directory %s: %w", bck, dir, err)
		}
		num++
	}
	return num, nil
}

func (mi *MountpathInfo) _setDisks(fsdisks ios.FsDisks) {
	mi.Disks = make([]string, len(fsdisks))
	i := 0
	for d := range fsdisks {
		mi.Disks[i] = d
		i++
	}
}

// available/used capacity

func (mi *MountpathInfo) getCapacity(config *cmn.Config, refresh bool) (c Capacity, err error) {
	if !refresh {
		mi.cmu.RLock()
		c = mi.capacity
		mi.cmu.RUnlock()
		return
	}

	mi.cmu.Lock()
	statfs := &syscall.Statfs_t{}
	if err = syscall.Statfs(mi.Path, statfs); err != nil {
		mi.cmu.Unlock()
		return
	}
	bused := statfs.Blocks - statfs.Bavail
	pct := bused * 100 / statfs.Blocks
	if pct >= uint64(config.LRU.HighWM)-1 {
		fpct := math.Ceil(float64(bused) * 100 / float64(statfs.Blocks))
		pct = uint64(fpct)
	}
	mi.capacity.Used = bused * uint64(statfs.Bsize)
	mi.capacity.Avail = statfs.Bavail * uint64(statfs.Bsize)
	mi.capacity.PctUsed = int32(pct)
	c = mi.capacity
	mi.cmu.Unlock()
	return
}

//
// mountpath add/enable helpers - always call under mfs lock
//

func (mi *MountpathInfo) _checkExists(availablePaths MPI) (err error) {
	if existingMi, exists := availablePaths[mi.Path]; exists {
		err = fmt.Errorf("failed adding %s: %s already exists", mi, existingMi)
	} else if existingPath, exists := mfs.fsIDs[mi.FsID]; exists && mfs.checkFsID {
		err = fmt.Errorf("FSID %v: filesystem sharing is not allowed: %s vs %q", mi.FsID, mi, existingPath)
	}
	return
}

func (mi *MountpathInfo) _addEnabled(tid string, availablePaths MPI) error {
	disks, err := mfs.ios.AddMpath(mi.Path, mi.Fs)
	if err != nil {
		return err
	}
	if tid != "" && cmn.GCO.Get().MDWrite != cmn.WriteNever {
		if err := mi.SetDaemonIDXattr(tid); err != nil {
			return err
		}
	}
	mi._setDisks(disks)
	_ = mi._string()
	availablePaths[mi.Path] = mi
	return nil
}

// with cloning
func (mi *MountpathInfo) addEnabledDisabled(tid string, enabled bool) (err error) {
	availablePaths, disabledPaths := cloneMPI()
	if enabled {
		if err = mi._checkExists(availablePaths); err != nil {
			return
		}
		if err = mi._addEnabled(tid, availablePaths); err != nil {
			return
		}
	} else {
		disabledPaths[mi.Path] = mi
	}
	mfs.fsIDs[mi.FsID] = mi.Path
	updatePaths(availablePaths, disabledPaths)
	return
}

///////////////
// MountedFS //
///////////////

// create a new singleton
func Init(iostater ...ios.IOStater) {
	mfs = &MountedFS{fsIDs: make(map[cos.FsID]string, 10), checkFsID: true}
	if len(iostater) > 0 {
		mfs.ios = iostater[0]
	} else {
		mfs.ios = ios.NewIostatContext()
	}
}

// InitMpaths prepares, validates, and adds configured mountpaths.
func InitMpaths(tid string) (changed bool, err error) {
	var (
		vmd         *VMD
		configPaths = cmn.GCO.Get().FSpaths.Paths
	)
	if len(configPaths) == 0 {
		err = fmt.Errorf("no fspaths - see README => Configuration and fspaths section in the config.sh")
		return
	}
	if vmd, err = initVMD(configPaths); err != nil {
		return
	}
	//
	// create mountpaths and load VMD
	//
	var (
		availablePaths = make(MPI, len(configPaths))
		disabledPaths  = make(MPI)
		cfgpaths       = configPaths.ToSlice()
		config         = cmn.GCO.Get()
	)
	if vmd == nil {
		glog.Warningf("no VMD: populating from the config %v", cfgpaths)
	} else {
		glog.Infof("loaded VMD %s: validating vs config %v", vmd, cfgpaths)
	}

	// populate under lock
	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	// no VMD
	if vmd == nil {
		for path := range configPaths {
			var mi *MountpathInfo
			if mi, err = newMountpath(path, tid); err != nil {
				return
			}
			if err = mi._checkExists(availablePaths); err != nil {
				return
			}
			if err = mi._addEnabled(tid, availablePaths); err != nil {
				return
			}
			if len(mi.Disks) == 0 && !config.TestingEnv() {
				err = &ErrMpathNoDisks{mi}
				return
			}
		}
		updatePaths(availablePaths, disabledPaths)
		_, err = CreateNewVMD(tid)
		return
	}

	// existing VMD
	if vmd.DaemonID != tid {
		err = newVMDIDMismatchErr(vmd, tid)
		return
	}
	for path := range configPaths {
		var (
			mi      *MountpathInfo
			enabled bool
		)
		if mpath, exists := vmd.Mountpaths[path]; !exists {
			enabled = true
			changed = true
			glog.Error(newVMDMissingMpathErr(path))
		} else {
			enabled = mpath.Enabled
		}
		if mi, err = newMountpath(path, tid); err == nil {
			if enabled {
				if err = mi._checkExists(availablePaths); err == nil {
					if err = mi._addEnabled(tid, availablePaths); err == nil {
						if len(mi.Disks) == 0 && !config.TestingEnv() {
							err = &ErrMpathNoDisks{mi}
						}
					}
				}
			} else {
				disabledPaths[mi.Path] = mi
			}
			mfs.fsIDs[mi.FsID] = mi.Path
		}
		if err != nil {
			return
		}
		if mi.Path != path {
			glog.Warningf("%s: cleanpath(%q) => %q", mi, path, mi.Path)
		}
	}
	updatePaths(availablePaths, disabledPaths)

	if len(vmd.Mountpaths) > len(configPaths) {
		for mpath := range vmd.Mountpaths {
			if !configPaths.Contains(mpath) {
				changed = true
				glog.Error(newConfigMissingMpathErr(mpath))
			}
		}
	}
	if changed {
		_, err = CreateNewVMD(tid)
	}
	return
}

func Decommission(mdOnly bool) {
	available, disabled := Get()
	allmpi := []MPI{available, disabled}
	for idx, mpi := range allmpi {
		if mdOnly { // NOTE: removing daemon ID below as well
			for _, mi := range mpi {
				mi.ClearMDs()
			}
		} else {
			// NOTE: the entire content including user data, MDs, and daemon ID
			for _, mi := range mpi {
				if err := os.RemoveAll(mi.Path); err != nil && !os.IsNotExist(err) && idx == 0 {
					// available is [0]
					glog.Errorf("failed to cleanup available %s: %v", mi, err)
				}
			}
		}
	}
	if mdOnly {
		RemoveDaemonIDs()
	}
}

//////////////////////////////
// `ios` package delegators //
//////////////////////////////

func GetAllMpathUtils() (utils *ios.MpathsUtils) { return mfs.ios.GetAllMpathUtils() }
func GetMpathUtil(mpath string) int64            { return mfs.ios.GetMpathUtil(mpath) }
func FillDiskStats(m ios.AllDiskStats)           { mfs.ios.FillDiskStats(m) }

// DisableFsIDCheck disables fsid checking when adding new mountpath
func DisableFsIDCheck() { mfs.checkFsID = false }

// Returns number of available mountpaths
func NumAvail() int {
	availablePaths := (*MPI)(mfs.available.Load())
	return len(*availablePaths)
}

func updatePaths(available, disabled MPI) {
	mfs.available.Store(unsafe.Pointer(&available))
	mfs.disabled.Store(unsafe.Pointer(&disabled))
}

// cloneMPI returns a shallow copy of the current (available, disabled) mountpaths
func cloneMPI() (MPI, MPI) {
	availablePaths, disabledPaths := Get()
	availableCopy := make(MPI, len(availablePaths))
	disabledCopy := make(MPI, len(availablePaths))

	for mpath, mpathInfo := range availablePaths {
		availableCopy[mpath] = mpathInfo
	}
	for mpath, mpathInfo := range disabledPaths {
		disabledCopy[mpath] = mpathInfo
	}
	return availableCopy, disabledCopy
}

// (used only in tests - compare with AddMpath below)
func Add(mpath, tid string) (mi *MountpathInfo, err error) {
	mi, err = newMountpath(mpath, tid)
	if err != nil {
		return
	}
	mfs.mu.Lock()
	err = mi.addEnabledDisabled(tid, true /*enabled*/)
	mfs.mu.Unlock()
	return
}

// Add adds new mountpath to the target's mountpaths.
func AddMpath(mpath, tid string, cb func()) (mi *MountpathInfo, err error) {
	debug.Assert(tid != "")
	mi, err = newMountpath(mpath, tid)
	if err != nil {
		return
	}

	mfs.mu.Lock()
	err = mi.addEnabledDisabled(tid, true /*enabled*/)
	if err == nil && len(mi.Disks) == 0 {
		if !cmn.GCO.Get().TestingEnv() {
			err = &ErrMpathNoDisks{mi}
		}
	}
	if err == nil {
		cb()
	}
	mfs.mu.Unlock()

	if mi.Path != mpath {
		glog.Warningf("%s: cleanpath(%q) => %q", mi, mpath, mi.Path)
	}
	return
}

// (used only in tests - compare with EnableMpath below)
func Enable(mpath string) (enabledMpath *MountpathInfo, err error) {
	var cleanMpath string
	if cleanMpath, err = cmn.ValidateMpath(mpath); err != nil {
		return
	}
	mfs.mu.Lock()
	enabledMpath, err = enable(mpath, cleanMpath, "" /*tid*/)
	mfs.mu.Unlock()
	return
}

// Enable enables previously disabled mountpath. enabled is set to
// true if mountpath has been moved from disabled to available and exists is
// set to true if such mountpath even exists.
func EnableMpath(mpath, tid string, cb func()) (enabledMpath *MountpathInfo, err error) {
	var cleanMpath string
	debug.Assert(tid != "")
	if cleanMpath, err = cmn.ValidateMpath(mpath); err != nil {
		return
	}
	mfs.mu.Lock()
	enabledMpath, err = enable(mpath, cleanMpath, tid)
	if err == nil {
		cb()
	}
	mfs.mu.Unlock()
	return
}

func enable(mpath, cleanMpath, tid string) (enabledMpath *MountpathInfo, err error) {
	availablePaths, disabledPaths := cloneMPI()
	if _, ok := availablePaths[cleanMpath]; ok { // nothing to do
		_, ok = disabledPaths[cleanMpath]
		debug.Assert(!ok)
		return
	}
	mi, ok := disabledPaths[cleanMpath]
	if !ok {
		err = cmn.NewNoMountpathError(mpath)
		return
	}
	debug.Assert(cleanMpath == mi.Path)

	// TODO: check tid == on-disk-tid if exists

	if err = mi._checkExists(availablePaths); err != nil {
		return
	}
	if err = mi._addEnabled(tid, availablePaths); err != nil {
		return
	}
	enabledMpath = mi
	delete(disabledPaths, cleanMpath)
	mfs.fsIDs[mi.FsID] = mi.Path
	updatePaths(availablePaths, disabledPaths)
	return
}

// Remove removes mountpaths from the target's mountpaths. It searches
// for the mountpath in `available` and, if not found, in `disabled`.
func Remove(mpath string, cb ...func()) (*MountpathInfo, error) {
	cleanMpath, err := cmn.ValidateMpath(mpath)
	if err != nil {
		return nil, err
	}

	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	// Clear daemonID xattr if set
	if err := removeXattr(cleanMpath, daemonIDXattr); err != nil {
		return nil, err
	}

	var (
		exists    bool
		mpathInfo *MountpathInfo

		availablePaths, disabledPaths = cloneMPI()
	)
	if mpathInfo, exists = availablePaths[cleanMpath]; !exists {
		if mpathInfo, exists = disabledPaths[cleanMpath]; !exists {
			return nil, fmt.Errorf("tried to remove non-existing mountpath: %v", mpath)
		}

		delete(disabledPaths, cleanMpath)
		delete(mfs.fsIDs, mpathInfo.FsID)
		updatePaths(availablePaths, disabledPaths)
		return mpathInfo, nil
	}

	mfs.ios.RemoveMpath(cleanMpath)
	delete(availablePaths, cleanMpath)
	delete(mfs.fsIDs, mpathInfo.FsID)

	availCnt := len(availablePaths)
	if availCnt == 0 {
		glog.Errorf("removed the last available mountpath %s", mpathInfo)
	} else {
		glog.Infof("removed mountpath %s (%d remain(s) active)", mpathInfo, availCnt)
	}

	moveMarkers(availablePaths, mpathInfo)
	updatePaths(availablePaths, disabledPaths)

	if availCnt > 0 && len(cb) > 0 {
		cb[0]()
	}
	return mpathInfo, nil
}

// Disable disables an available mountpath.
// It returns disabled mountpath if it was actually disabled - moved from enabled to disabled.
// Otherwise it returns nil, even if the mountpath existed (but was already disabled).
func Disable(mpath string, cb ...func()) (disabledMpath *MountpathInfo, err error) {
	cleanMpath, err := cmn.ValidateMpath(mpath)
	if err != nil {
		return nil, err
	}

	mfs.mu.Lock()
	defer mfs.mu.Unlock()

	availablePaths, disabledPaths := cloneMPI()
	if mpathInfo, ok := availablePaths[cleanMpath]; ok {
		disabledPaths[cleanMpath] = mpathInfo
		mfs.ios.RemoveMpath(cleanMpath)
		delete(availablePaths, cleanMpath)
		moveMarkers(availablePaths, mpathInfo)
		updatePaths(availablePaths, disabledPaths)
		if l := len(availablePaths); l == 0 {
			glog.Errorf("disabled the last available mountpath %s", mpathInfo)
		} else {
			if len(cb) > 0 {
				cb[0]()
			}
			glog.Infof("disabled mountpath %s (%d remain(s) active)", mpathInfo, l)
		}
		return mpathInfo, nil
	}

	if _, ok := disabledPaths[cleanMpath]; ok {
		return nil, nil
	}
	return nil, cmn.NewNoMountpathError(mpath)
}

// Mountpaths returns both available and disabled mountpaths.
func Get() (MPI, MPI) {
	var (
		availablePaths = (*MPI)(mfs.available.Load())
		disabledPaths  = (*MPI)(mfs.disabled.Load())
	)
	if availablePaths == nil {
		tmp := make(MPI, 10)
		availablePaths = &tmp
	}
	if disabledPaths == nil {
		tmp := make(MPI, 10)
		disabledPaths = &tmp
	}
	return *availablePaths, *disabledPaths
}

func CreateBucket(op string, bck cmn.Bck, nilbmd bool) (errs []error) {
	var (
		availablePaths, _ = Get()
		totalDirs         = len(availablePaths) * len(CSM.RegisteredContentTypes)
		totalCreatedDirs  int
	)
	for _, mi := range availablePaths {
		num, err := mi.createBckDirs(bck, nilbmd)
		if err != nil {
			errs = append(errs, err)
		} else {
			totalCreatedDirs += num
		}
	}
	if errs == nil {
		debug.Assert(totalCreatedDirs == totalDirs)
		if glog.FastV(4, glog.SmoduleFS) {
			glog.Infof("%s(create bucket dirs): %s, num=%d", op, bck, totalDirs)
		}
	}
	return
}

func DestroyBucket(op string, bck cmn.Bck, bid uint64) error {
	const destroyStr = "destroy-ais-bucket-dir"
	var n int
	availablePaths, _ := Get()
	for _, mi := range availablePaths {
		dir := mi.makeDelPathBck(bck, bid)
		if err := mi.MoveToTrash(dir); err != nil {
			glog.Errorf("%s: failed to %s (dir: %q, err: %v)", op, destroyStr, dir, err)
		} else {
			n++
		}
	}
	if count := len(availablePaths); n < count {
		return fmt.Errorf("bucket %s: failed to destroy %d/%d dirs", bck, count-n, count)
	}
	return nil
}

func RenameBucketDirs(bidFrom uint64, bckFrom, bckTo cmn.Bck) (err error) {
	availablePaths, _ := Get()
	renamed := make([]*MountpathInfo, 0, len(availablePaths))
	for _, mi := range availablePaths {
		fromPath := mi.makeDelPathBck(bckFrom, bidFrom)
		toPath := mi.MakePathBck(bckTo)
		// os.Rename fails when renaming to a directory which already exists.
		// We should remove destination bucket directory before rename. It's reasonable to do so
		// as all targets agreed to rename and rename was committed in BMD.
		os.RemoveAll(toPath)
		if err = os.Rename(fromPath, toPath); err != nil {
			break
		}
		renamed = append(renamed, mi)
	}

	if err == nil {
		return
	}
	for _, mi := range renamed {
		fromPath := mi.MakePathBck(bckTo)
		toPath := mi.MakePathBck(bckFrom)
		if erd := os.Rename(fromPath, toPath); erd != nil {
			glog.Error(erd)
		}
	}
	return
}

func moveMarkers(available MPI, from *MountpathInfo) {
	var (
		fromPath    = filepath.Join(from.Path, cmn.MarkersDirName)
		finfos, err = os.ReadDir(fromPath)
	)
	if err != nil {
		if !os.IsNotExist(err) {
			glog.Errorf("Failed to read markers directory %q: %v", fromPath, err)
		}
		return
	}
	if len(finfos) == 0 {
		return // no markers, nothing to do
	}

	// NOTE: `from` path must no longer be in the available mountpaths
	_, ok := available[from.Path]
	debug.AssertMsg(!ok, from.String())
	for _, mpath := range available {
		ok = true
		for _, fi := range finfos {
			debug.AssertMsg(!fi.IsDir(), cmn.MarkersDirName+"/"+fi.Name()) // marker is file
			var (
				fromPath = filepath.Join(from.Path, cmn.MarkersDirName, fi.Name())
				toPath   = filepath.Join(mpath.Path, cmn.MarkersDirName, fi.Name())
			)
			_, _, err := cos.CopyFile(fromPath, toPath, nil, cos.ChecksumNone)
			if err != nil && os.IsNotExist(err) {
				glog.Errorf("Failed to move marker %q to %q: %v)", fromPath, toPath, err)
				ok = false
			}
		}
		if ok {
			break
		}
	}
	from.ClearMDs()
}

// capacity management

func GetCapStatus() (cs CapStatus) {
	mfs.cmu.RLock()
	cs = mfs.capStatus
	mfs.cmu.RUnlock()
	return
}

func RefreshCapStatus(config *cmn.Config, mpcap MPCap) (cs CapStatus, err error) {
	var (
		availablePaths, _ = Get()
		c                 Capacity
	)
	if len(availablePaths) == 0 {
		err = ErrNoMountpaths
		return
	}
	if config == nil {
		config = cmn.GCO.Get()
	}
	high, oos := config.LRU.HighWM, config.LRU.OOS
	for path, mi := range availablePaths {
		if c, err = mi.getCapacity(config, true); err != nil {
			glog.Error(err) // TODO: handle
			return
		}
		cs.TotalUsed += c.Used
		cs.TotalAvail += c.Avail
		cs.PctMax = cos.MaxI32(cs.PctMax, c.PctUsed)
		cs.PctAvg += c.PctUsed
		if mpcap != nil {
			mpcap[path] = c
		}
	}
	cs.PctAvg /= int32(len(availablePaths))
	cs.OOS = int64(cs.PctMax) > oos
	if cs.OOS || int64(cs.PctMax) > high {
		cs.Err = cmn.NewErrorCapacityExceeded(high, cs.PctMax, cs.TotalUsed, cs.TotalAvail+cs.TotalUsed, cs.OOS)
	}
	// cached cap state
	mfs.cmu.Lock()
	mfs.capStatus = cs
	mfs.capTime.curr = mono.NanoTime()
	mfs.capTime.next = mfs.capTime.curr + int64(nextRefresh(config))
	mfs.cmu.Unlock()
	return
}

// recompute next time to refresh cached capacity stats (mfs.capStatus)
func nextRefresh(config *cmn.Config) time.Duration {
	var (
		util = int64(mfs.capStatus.PctAvg) // NOTE: average not max
		umin = cos.MaxI64(config.LRU.HighWM-10, config.LRU.LowWM)
		umax = config.LRU.OOS
		tmax = config.LRU.CapacityUpdTime.D()
		tmin = config.Periodic.StatsTime.D()
	)
	if util <= umin {
		return tmax
	}
	if util >= umax {
		return tmin
	}
	debug.Assert(umin < umax)
	debug.Assert(tmin < tmax)
	ratio := (util - umin) * 100 / (umax - umin)
	return time.Duration(ratio)*(tmax-tmin)/100 + tmin
}

// NOTE: Is called only and exclusively by `stats.Trunner` providing
//  `config.Periodic.StatsTime` tick.
func CapPeriodic(mpcap MPCap) (cs CapStatus, updated bool, err error) {
	config := cmn.GCO.Get()
	mfs.cmu.RLock()
	mfs.capTime.curr += int64(config.Periodic.StatsTime)
	if mfs.capTime.curr < mfs.capTime.next {
		mfs.cmu.RUnlock()
		return
	}
	mfs.cmu.RUnlock()
	cs, err = RefreshCapStatus(config, mpcap)
	updated = true
	return
}

func CapStatusAux() (fsInfo cmn.CapacityInfo) {
	cs := GetCapStatus()
	fsInfo.Used = cs.TotalUsed
	fsInfo.Total = cs.TotalUsed + cs.TotalAvail
	fsInfo.PctUsed = float64(cs.PctAvg)
	return
}
