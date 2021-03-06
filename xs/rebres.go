// Package xs contains eXtended actions (xactions) except storage services
// (mirror, ec) and extensions (downloader, lru).
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xreg"
)

// rebalance & resilver xactions

type (
	getMarked  = func() xaction.XactMarked
	rebFactory struct {
		xreg.RenewBase
		xact *Rebalance
		args *xreg.RebalanceArgs
	}
	Rebalance struct {
		xaction.XactBase
		statTracker  stats.Tracker // extended stats
		getRebMarked getMarked
	}
	rslvrFactory struct {
		xreg.RenewBase
		xact *Resilver
	}
	Resilver struct {
		xaction.XactBase
	}
)

// interface guard
var (
	_ cluster.Xact   = (*Rebalance)(nil)
	_ xreg.Renewable = (*rebFactory)(nil)
	_ cluster.Xact   = (*Resilver)(nil)
	_ xreg.Renewable = (*rslvrFactory)(nil)
)

///////////////
// Rebalance //
///////////////

func (*rebFactory) New(args xreg.Args, _ *cluster.Bck) xreg.Renewable {
	return &rebFactory{RenewBase: xreg.RenewBase{Args: args}, args: args.Custom.(*xreg.RebalanceArgs)}
}

func (p *rebFactory) Start() error {
	p.xact = NewRebalance(p.args.ID, p.Kind(), p.args.StatTracker, xreg.GetRebMarked)
	return nil
}

func (*rebFactory) Kind() string        { return cmn.ActRebalance }
func (p *rebFactory) Get() cluster.Xact { return p.xact }

func (p *rebFactory) WhenPrevIsRunning(prevEntry xreg.Renewable) (wpr xreg.WPR, err error) {
	xreb := prevEntry.(*rebFactory)
	wpr = xreg.WprAbort
	if xreb.args.ID > p.args.ID {
		glog.Errorf("(reb: %s) %s is greater than %s", xreb.xact, xreb.args.ID, p.args.ID)
		wpr = xreg.WprUse
	} else if xreb.args.ID == p.args.ID {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("%s already running, nothing to do", xreb.xact)
		}
		wpr = xreg.WprUse
	}
	return
}

func NewRebalance(id, kind string, statTracker stats.Tracker, getMarked getMarked) (xact *Rebalance) {
	xact = &Rebalance{statTracker: statTracker, getRebMarked: getMarked}
	xact.InitBase(id, kind, nil)
	return
}

func (*Rebalance) Run() { debug.Assert(false) }

// override/extend cmn.XactBase.Stats()
func (xact *Rebalance) Stats() cluster.XactStats {
	var (
		baseStats   = xact.XactBase.Stats().(*xaction.BaseXactStats)
		rebStats    = stats.RebalanceTargetStats{BaseXactStats: *baseStats}
		statsRunner = xact.statTracker
	)
	rebStats.Ext.RebTxCount = statsRunner.Get(stats.RebTxCount)
	rebStats.Ext.RebTxSize = statsRunner.Get(stats.RebTxSize)
	rebStats.Ext.RebRxCount = statsRunner.Get(stats.RebRxCount)
	rebStats.Ext.RebRxSize = statsRunner.Get(stats.RebRxSize)
	if marked := xact.getRebMarked(); marked.Xact != nil {
		var err error
		rebStats.Ext.RebID, err = xaction.S2RebID(marked.Xact.ID())
		debug.AssertNoErr(err)
	} else {
		rebStats.Ext.RebID = 0
	}
	rebStats.ObjCountX = rebStats.Ext.RebTxCount + rebStats.Ext.RebRxCount
	rebStats.BytesCountX = rebStats.Ext.RebTxSize + rebStats.Ext.RebRxSize
	return &rebStats
}

//////////////
// Resilver //
//////////////

func (*rslvrFactory) New(args xreg.Args, _ *cluster.Bck) xreg.Renewable {
	return &rslvrFactory{RenewBase: xreg.RenewBase{Args: args}}
}

func (p *rslvrFactory) Start() error {
	p.xact = NewResilver(p.UUID, p.Kind())
	return nil
}

func (*rslvrFactory) Kind() string                                       { return cmn.ActResilver }
func (p *rslvrFactory) Get() cluster.Xact                                { return p.xact }
func (*rslvrFactory) WhenPrevIsRunning(xreg.Renewable) (xreg.WPR, error) { return xreg.WprAbort, nil }

func NewResilver(id, kind string) (xact *Resilver) {
	xact = &Resilver{}
	xact.InitBase(id, kind, nil)
	return
}

func (*Resilver) Run() { debug.Assert(false) }
