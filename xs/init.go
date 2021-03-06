// Package xs contains eXtended actions (xactions) except storage services
// (mirror, ec) and extensions (downloader, lru).
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/xreg"
)

// for additional startup-time reg-s see lru, downloader, ec
func init() {
	xreg.RegGlobXact(&eleFactory{})
	xreg.RegGlobXact(&rslvrFactory{})
	xreg.RegGlobXact(&rebFactory{})

	xreg.RegBckXact(&MovFactory{})
	xreg.RegBckXact(&evdFactory{kind: cmn.ActEvictObjects})
	xreg.RegBckXact(&evdFactory{kind: cmn.ActDelete})
	xreg.RegBckXact(&prfFactory{})

	xreg.RegBckXact(&olFactory{})

	xreg.RegBckXact(&proFactory{})
	xreg.RegBckXact(&llcFactory{})
	xreg.RegBckXact(&archFactory{})
}
