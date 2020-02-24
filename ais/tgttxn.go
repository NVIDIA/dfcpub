// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2020, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/mirror"
	"github.com/NVIDIA/aistore/xaction"
	jsoniter "github.com/json-iterator/go"
)

// convenience structure to gather all (or most) of the relevant context in one place
// (compare with txnClientCtx & prepTxnClient)
type txnServerCtx struct {
	uuid    string
	timeout time.Duration
	phase   string
	smapVer int64
	bmdVer  int64
	msgInt  *actionMsgInternal
	caller  string
	bck     *cluster.Bck
}

// verb /v1/txn
func (t *targetrunner) txnHandler(w http.ResponseWriter, r *http.Request) {
	// 1. check
	if r.Method != http.MethodPost {
		cmn.InvalidHandlerWithMsg(w, r, "invalid method for /txn path")
		return
	}
	msgInt := &actionMsgInternal{}
	if cmn.ReadJSON(w, r, msgInt) != nil {
		return
	}
	apiItems, err := t.checkRESTItems(w, r, 0, true, cmn.Version, cmn.Txn)
	if err != nil {
		return
	}
	if len(apiItems) < 2 {
		t.invalmsghdlr(w, r, "url too short: expecting bucket and txn phase", http.StatusBadRequest)
		return
	}
	// 2. gather all context
	c, err := t.prepTxnServer(r, msgInt, apiItems)
	if err != nil {
		t.invalmsghdlr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	// 3. do
	switch msgInt.Action {
	case cmn.ActCreateLB, cmn.ActRegisterCB:
		if err = t.createBucket(c); err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
	case cmn.ActMakeNCopies:
		if err = t.makeNCopies(c); err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
	case cmn.ActSetBprops:
		if err = t.setBucketProps(c); err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
	case cmn.ActRenameLB:
		if err = t.renameBucket(c); err != nil {
			t.invalmsghdlr(w, r, err.Error())
		}
	default:
		t.invalmsghdlr(w, r, fmt.Sprintf(fmtUnknownAct, msgInt))
	}
}

//////////////////
// createBucket //
//////////////////

func (t *targetrunner) createBucket(c *txnServerCtx) error {
	switch c.phase {
	case cmn.ActBegin:
		txn := newTxnCreateBucket(c)
		if err := t.transactions.begin(txn); err != nil {
			return err
		}
	case cmn.ActAbort:
		t.transactions.find(c.uuid, true /* remove */)
	case cmn.ActCommit:
		txn, err := t.transactions.find(c.uuid, false)
		if err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		// wait for newBMD w/timeout
		if err = t.transactions.wait(txn, c.timeout); err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
	default:
		cmn.Assert(false)
	}
	return nil
}

/////////////////
// makeNCopies //
/////////////////

func (t *targetrunner) makeNCopies(c *txnServerCtx) error {
	if err := c.bck.Init(t.owner.bmd, t.si); err != nil {
		return err
	}
	switch c.phase {
	case cmn.ActBegin:
		curCopies, newCopies, err := t.validateMakeNCopies(c.bck, c.msgInt)
		if err != nil {
			return err
		}
		txn := newTxnMakeNCopies(c, curCopies, newCopies)
		if err := t.transactions.begin(txn); err != nil {
			return err
		}
	case cmn.ActAbort:
		t.transactions.find(c.uuid, true /* remove */)
	case cmn.ActCommit:
		copies, _ := t.parseNCopies(c.msgInt.Value)
		txn, err := t.transactions.find(c.uuid, false)
		if err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		txnMnc := txn.(*txnMakeNCopies)
		cmn.Assert(txnMnc.newCopies == copies)
		// wait for newBMD w/timeout
		if err = t.transactions.wait(txn, c.timeout); err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		// do the work in xaction
		xaction.Registry.DoAbort(cmn.ActPutCopies, c.bck)
		xaction.Registry.RenewBckMakeNCopies(c.bck, t, int(copies))
	default:
		cmn.Assert(false)
	}
	return nil
}

func (t *targetrunner) validateMakeNCopies(bck *cluster.Bck, msgInt *actionMsgInternal) (curCopies, newCopies int64, err error) {
	curCopies = bck.Props.Mirror.Copies
	newCopies, err = t.parseNCopies(msgInt.Value)
	if err == nil {
		err = mirror.ValidateNCopies(t.si.Name(), int(newCopies))
	}
	if err != nil {
		return
	}
	// don't allow increasing num-copies when used cap is above high wm (let alone OOS)
	if bck.Props.Mirror.Copies < newCopies {
		capInfo := t.AvgCapUsed(nil)
		err = capInfo.Err
	}
	return
}

////////////////////
// setBucketProps //
////////////////////

func (t *targetrunner) setBucketProps(c *txnServerCtx) error {
	if err := c.bck.Init(t.owner.bmd, t.si); err != nil {
		return err
	}
	switch c.phase {
	case cmn.ActBegin:
		var (
			nprops *cmn.BucketProps
			err    error
		)
		if nprops, err = t.validateNprops(c.bck, c.msgInt); err != nil {
			return err
		}
		txn := newTxnSetBucketProps(c, nprops)
		if err := t.transactions.begin(txn); err != nil {
			return err
		}
	case cmn.ActAbort:
		t.transactions.find(c.uuid, true /* remove */)
	case cmn.ActCommit:
		txn, err := t.transactions.find(c.uuid, false)
		if err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		txnSetBprops := txn.(*txnSetBucketProps)
		// wait for newBMD w/timeout
		if err = t.transactions.wait(txn, c.timeout); err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		if remirror(txnSetBprops.bprops, txnSetBprops.nprops) {
			xaction.Registry.DoAbort(cmn.ActPutCopies, c.bck)
			xaction.Registry.RenewBckMakeNCopies(c.bck, t, int(txnSetBprops.nprops.Mirror.Copies))
		}
	default:
		cmn.Assert(false)
	}
	return nil
}

func (t *targetrunner) validateNprops(bck *cluster.Bck, msgInt *actionMsgInternal) (nprops *cmn.BucketProps, err error) {
	var (
		body    = cmn.MustMarshal(msgInt.Value)
		capInfo = t.AvgCapUsed(cmn.GCO.Get())
	)
	nprops = &cmn.BucketProps{}
	if err = jsoniter.Unmarshal(body, nprops); err != nil {
		return
	}
	if nprops.Mirror.Enabled {
		mpathCount := fs.Mountpaths.NumAvail()
		if int(nprops.Mirror.Copies) > mpathCount {
			err = fmt.Errorf("%s: number of mountpaths %d is insufficient to configure %s as a %d-way mirror",
				t.si, mpathCount, bck, nprops.Mirror.Copies)
			return
		}
		if nprops.Mirror.Copies > bck.Props.Mirror.Copies && capInfo.Err != nil {
			return nprops, capInfo.Err
		}
	}
	if nprops.EC.Enabled && !bck.Props.EC.Enabled {
		err = capInfo.Err
	}
	return
}

func remirror(bprops, nprops *cmn.BucketProps) bool {
	if !bprops.Mirror.Enabled && nprops.Mirror.Enabled {
		return true
	}
	if bprops.Mirror.Enabled && nprops.Mirror.Enabled {
		return bprops.Mirror.Copies != nprops.Mirror.Copies
	}
	return false
}

//////////////////
// renameBucket //
//////////////////

func (t *targetrunner) renameBucket(c *txnServerCtx) error {
	if err := c.bck.Init(t.owner.bmd, t.si); err != nil {
		return err
	}
	switch c.phase {
	case cmn.ActBegin:
		var (
			bckTo   *cluster.Bck
			bckFrom = c.bck
			err     error
		)
		if bckTo, err = t.validateBckRenTxn(bckFrom, c.msgInt); err != nil {
			return err
		}
		txn := newTxnRenameBucket(c, bckFrom, bckTo)
		if err := t.transactions.begin(txn); err != nil {
			return err
		}
	case cmn.ActAbort:
		t.transactions.find(c.uuid, true /* remove */)
	case cmn.ActCommit:
		var xact *xaction.FastRen
		txn, err := t.transactions.find(c.uuid, false)
		if err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		txnRenB := txn.(*txnRenameBucket)
		// wait for newBMD w/timeout
		if err = t.transactions.wait(txn, c.timeout); err != nil {
			return fmt.Errorf("%s %s: %v", t.si, txn, err)
		}
		xact, err = xaction.Registry.RenewBckFastRename(t, txnRenB.bckFrom, txnRenB.bckTo, cmn.ActCommit, t.rebManager)
		if err != nil {
			return err // must not happen at commit time
		}

		err = fs.Mountpaths.RenameBucketDirs(txnRenB.bckFrom.Bck, txnRenB.bckTo.Bck)
		if err != nil {
			return err // ditto
		}

		globalRebID := c.msgInt.RMDVersion
		cmn.Assert(globalRebID > 0)

		t.gfn.local.Activate()
		t.gfn.global.activateTimed()
		waiter := &sync.WaitGroup{}
		waiter.Add(1)
		go xact.Run(waiter, globalRebID) // FIXME: #654
		waiter.Wait()

		time.Sleep(200 * time.Millisecond) // FIXME: !1727, #654
	default:
		cmn.Assert(false)
	}
	return nil
}

func (t *targetrunner) validateBckRenTxn(bckFrom *cluster.Bck, msgInt *actionMsgInternal) (bckTo *cluster.Bck, err error) {
	var (
		bTo               = &cmn.Bck{}
		body              = cmn.MustMarshal(msgInt.Value)
		config            = cmn.GCO.Get()
		availablePaths, _ = fs.Mountpaths.Get()
	)
	if err = jsoniter.Unmarshal(body, bTo); err != nil {
		return
	}
	if capInfo := t.AvgCapUsed(config); capInfo.Err != nil {
		return nil, capInfo.Err
	}
	bckTo = cluster.NewBck(bTo.Name, bTo.Provider, bTo.Ns)
	bmd := t.owner.bmd.get()
	if _, present := bmd.Get(bckFrom); !present {
		return bckTo, cmn.NewErrorBucketDoesNotExist(bckFrom.Bck, t.si.String())
	}
	if _, present := bmd.Get(bckTo); present {
		return bckTo, cmn.NewErrorBucketAlreadyExists(bckTo.Bck, t.si.String())
	}
	for _, mpathInfo := range availablePaths {
		path := mpathInfo.MakePathCT(bckTo.Bck, fs.ObjectType)
		if err := fs.Access(path); err != nil {
			if !os.IsNotExist(err) {
				return bckTo, err
			}
			continue
		}
		if names, empty, err := fs.IsDirEmpty(path); err != nil {
			return bckTo, err
		} else if !empty {
			return bckTo, fmt.Errorf("directory %q already exists and is not empty (%v...)", path, names)
		}
	}
	return
}

//////////
// misc //
//////////

func (t *targetrunner) prepTxnServer(r *http.Request, msgInt *actionMsgInternal, apiItems []string) (*txnServerCtx, error) {
	var (
		bucket string
		err    error
		query  = r.URL.Query()
		c      = &txnServerCtx{}
	)
	c.msgInt = msgInt
	c.caller = r.Header.Get(cmn.HeaderCallerName)
	bucket, c.phase = apiItems[0], apiItems[1]
	if c.bck, err = newBckFromQuery(bucket, query); err != nil {
		return c, err
	}
	c.uuid = c.msgInt.TxnID
	if c.uuid == "" {
		return c, nil
	}
	c.timeout, err = cmn.S2Duration(query.Get(cmn.URLParamTxnTimeout))

	c.smapVer = t.owner.smap.get().version()
	c.bmdVer = t.owner.bmd.get().version()

	return c, err
}
