// Package api provides AIStore API over HTTP(S)
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	jsoniter "github.com/json-iterator/go"
)

const (
	initialPollInterval = 50 * time.Millisecond
	maxPollInterval     = 10 * time.Second
)

type (
	// ProgressInfo to notify a caller about an operation progress. The operation
	// returns negative values for data that unavailable(e.g, Total or Percent for ListObjects).
	ProgressInfo struct {
		Percent float64
		Count   int
		Total   int
	}
	ProgressContext struct {
		startTime time.Time // time when operation was started
		callAfter time.Time // call a callback only after this point in time
		callback  ProgressCallback

		info ProgressInfo
	}
	ProgressCallback = func(pi *ProgressContext)
)

// SetBucketProps sets the properties of a bucket.
// Validation of the properties passed in is performed by AIStore Proxy.
func SetBucketProps(baseParams BaseParams, bck cmn.Bck, props *cmn.BucketPropsToUpdate, query ...url.Values) (string, error) {
	b := cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActSetBprops, Value: props})
	return patchBucketProps(baseParams, bck, b, query...)
}

// ResetBucketProps resets the properties of a bucket to the global configuration.
func ResetBucketProps(baseParams BaseParams, bck cmn.Bck, query ...url.Values) (string, error) {
	b := cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActResetBprops})
	return patchBucketProps(baseParams, bck, b, query...)
}

func patchBucketProps(baseParams BaseParams, bck cmn.Bck, body []byte, query ...url.Values) (xactID string, err error) {
	var q url.Values
	if len(query) > 0 {
		q = query[0]
	}
	q = cmn.AddBckToQuery(q, bck)
	baseParams.Method = http.MethodPatch
	path := cmn.URLPathBuckets.Join(bck.Name)
	err = DoHTTPRequest(ReqParams{BaseParams: baseParams, Path: path, Body: body, Query: q}, &xactID)
	return
}

// HeadBucket returns the properties of a bucket specified by its name.
// Converts the string type fields returned from the HEAD request to their
// corresponding counterparts in the cmn.BucketProps struct.
func HeadBucket(baseParams BaseParams, bck cmn.Bck, query ...url.Values) (p *cmn.BucketProps, err error) {
	var (
		path = cmn.URLPathBuckets.Join(bck.Name)
		q    url.Values
	)
	p = &cmn.BucketProps{}
	baseParams.Method = http.MethodHead
	if len(query) > 0 {
		q = query[0]
	}
	q = cmn.AddBckToQuery(q, bck)

	resp, err := doHTTPRequestGetResp(ReqParams{BaseParams: baseParams, Path: path, Query: q}, nil)
	// try to fill in error message (HEAD response will never contain one)
	if err != nil {
		if httpErr := cmn.Err2HTTPErr(err); httpErr != nil {
			switch httpErr.Status {
			case http.StatusUnauthorized:
				httpErr.Message = fmt.Sprintf("Bucket %q unauthorized access", bck)
			case http.StatusForbidden:
				httpErr.Message = fmt.Sprintf("Bucket %q access denied", bck)
			case http.StatusNotFound:
				httpErr.Message = fmt.Sprintf("Bucket %q not found", bck)
			case http.StatusGone:
				httpErr.Message = fmt.Sprintf("Bucket %q has been removed from the backend", bck)
			default:
				httpErr.Message = fmt.Sprintf(
					"Failed to access bucket %q (code: %d)",
					bck, httpErr.Status,
				)
			}
			err = httpErr
		}
		return
	}
	err = jsoniter.Unmarshal([]byte(resp.Header.Get(cmn.HdrBucketProps)), &p)
	return p, err
}

// ListBuckets returns buckets for provided query.
func ListBuckets(baseParams BaseParams, queryBcks cmn.QueryBcks) (cmn.Bcks, error) {
	var (
		bcks  = cmn.Bcks{}
		path  = cmn.URLPathBuckets.S
		body  = cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActList})
		query = cmn.AddBckToQuery(nil, cmn.Bck(queryBcks))
	)

	baseParams.Method = http.MethodGet
	err := DoHTTPRequest(ReqParams{BaseParams: baseParams, Path: path, Body: body, Query: query}, &bcks)
	if err != nil {
		return nil, err
	}
	return bcks, nil
}

// GetBucketsSummaries returns bucket summaries for the specified backend provider
// (and all bucket summaries for unspecified ("") provider).
func GetBucketsSummaries(baseParams BaseParams, query cmn.QueryBcks,
	msg *cmn.BucketSummaryMsg) (cmn.BucketsSummaries, error) {
	if msg == nil {
		msg = &cmn.BucketSummaryMsg{}
	}
	baseParams.Method = http.MethodGet
	reqParams := ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(query.Name),
		Header:     http.Header{cmn.HdrContentType: []string{cmn.ContentJSON}},
		Query:      cmn.AddBckToQuery(nil, cmn.Bck(query)),
	}
	var summaries cmn.BucketsSummaries
	if err := waitForAsyncReqComplete(reqParams, cmn.ActSummary, msg, &summaries); err != nil {
		return nil, err
	}
	sort.Sort(summaries)
	return summaries, nil
}

// CreateBucket sends HTTP request to a proxy to create an AIS bucket with the given name.
func CreateBucket(baseParams BaseParams, bck cmn.Bck, props *cmn.BucketPropsToUpdate) error {
	if err := bck.Validate(); err != nil {
		return err
	}
	baseParams.Method = http.MethodPost
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(bck.Name),
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActCreateBck, Value: props}),
		Query:      cmn.AddBckToQuery(nil, bck),
	})
}

// DestroyBucket sends HTTP request to a proxy to remove an AIS bucket with the given name.
func DestroyBucket(baseParams BaseParams, bck cmn.Bck) error {
	baseParams.Method = http.MethodDelete
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(bck.Name),
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActDestroyBck}),
		Query:      cmn.AddBckToQuery(nil, bck),
	})
}

// DoesBucketExist queries a proxy or target to get a list of all AIS buckets,
// returns true if the bucket is present in the list.
func DoesBucketExist(baseParams BaseParams, query cmn.QueryBcks) (bool, error) {
	bcks, err := ListBuckets(baseParams, query)
	if err != nil {
		return false, err
	}
	return bcks.Contains(query), nil
}

// CopyBucket copies existing `fromBck` bucket to the destination `toBck` thus,
// effectively, creating a copy of the `fromBck`.
// * AIS will create `toBck` on the fly but only if the destination bucket does not
//   exist and _is_ provided by AIStore; 3rd party backend destination must exist -
//   otherwise the copy operation won't be successful.
// * There are no limitations on copying buckets across Backend providers:
//   you can copy AIS bucket to (or from) AWS bucket, and the latter to Google or Azure
//   bucket, etc.
// * Copying multiple buckets to the same destination bucket is also permitted.
func CopyBucket(baseParams BaseParams, fromBck, toBck cmn.Bck, optionalMsg ...*cmn.CopyBckMsg) (xactID string, err error) {
	if err = toBck.Validate(); err != nil {
		return
	}
	var msg *cmn.CopyBckMsg
	if len(optionalMsg) > 0 && optionalMsg[0] != nil {
		msg = optionalMsg[0]
	}
	q := cmn.AddBckToQuery(nil, fromBck)
	_ = cmn.AddBckUnameToQuery(q, toBck, cmn.URLParamBucketTo)
	baseParams.Method = http.MethodPost
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(fromBck.Name),
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActCopyBck, Value: msg}),
		Query:      q,
	}, &xactID)
	return
}

// RenameBucket renames fromBck as toBck.
func RenameBucket(baseParams BaseParams, fromBck, toBck cmn.Bck) (xactID string, err error) {
	if err = toBck.Validate(); err != nil {
		return
	}
	baseParams.Method = http.MethodPost
	q := cmn.AddBckToQuery(nil, fromBck)
	_ = cmn.AddBckUnameToQuery(q, toBck, cmn.URLParamBucketTo)
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(fromBck.Name),
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActMoveBck}),
		Query:      q,
	}, &xactID)
	return
}

// EvictRemoteBucket sends HTTP request to a proxy to evict an entire remote bucket from the AIStore
// - the operation results in eliminating all traces of the specified remote bucket in the AIStore
func EvictRemoteBucket(baseParams BaseParams, bck cmn.Bck, keepMD bool) error {
	var q url.Values
	baseParams.Method = http.MethodDelete
	if keepMD {
		q = url.Values{cmn.URLParamKeepBckMD: []string{"true"}}
	}
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(bck.Name),
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActEvictRemoteBck}),
		Query:      cmn.AddBckToQuery(q, bck),
	})
}

// Polling:
// 1. The function sends the requests as is (smsg.UUID should be empty) to initiate
//    asynchronous task. The destination returns ID of a newly created task
// 2. Starts polling: request destination with received UUID in a loop while
//    the destination returns StatusAccepted=task is still running
//	  Time between requests is dynamic: it starts at 200ms and increases
//	  by half after every "not-StatusOK" request. It is limited with 10 seconds
// 3. Breaks loop on error
// 4. If the destination returns status code StatusOK, it means the response
//    contains the real data and the function returns the response to the caller
func waitForAsyncReqComplete(reqParams ReqParams, action string, msg *cmn.BucketSummaryMsg, v interface{}) error {
	cos.Assert(action == cmn.ActSummary)
	var (
		uuid   string
		sleep  = initialPollInterval
		actMsg = cmn.ActionMsg{Action: action, Value: msg}
	)
	if reqParams.Query == nil {
		reqParams.Query = url.Values{}
	}
	reqParams.Body = cos.MustMarshal(actMsg)
	resp, err := doHTTPRequestGetResp(reqParams, &uuid)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		if resp.StatusCode == http.StatusOK {
			return errors.New("expected 202 response code on first call, got 200")
		}
		return fmt.Errorf("invalid response code: %d", resp.StatusCode)
	}
	if msg.UUID == "" {
		msg.UUID = uuid
	}

	// Poll async task for http.StatusOK completion
	for {
		reqParams.Body = cos.MustMarshal(actMsg)
		resp, err = doHTTPRequestGetResp(reqParams, v)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		time.Sleep(sleep)
		if sleep < maxPollInterval {
			sleep += sleep / 2
		}
	}
	return err
}

// ListObjects returns list of objects in a bucket. `numObjects` is the
// maximum number of objects returned (0 - return all objects in a bucket).
func ListObjects(baseParams BaseParams, bck cmn.Bck, smsg *cmn.SelectMsg, numObjects uint,
	args ...*ProgressContext) (bckList *cmn.BucketList, err error) {
	baseParams.Method = http.MethodGet
	if smsg == nil {
		smsg = &cmn.SelectMsg{}
	}

	// NOTE: No need to preallocate bucket entries slice, we use msgpack so it will do it for us!

	var (
		ctx *ProgressContext

		path      = cmn.URLPathBuckets.Join(bck.Name)
		hdr       = http.Header{cmn.HdrAccept: []string{cmn.ContentMsgPack}}
		q         = cmn.AddBckToQuery(url.Values{}, bck)
		reqParams = ReqParams{BaseParams: baseParams, Path: path, Header: hdr, Query: q}

		nextPage = &cmn.BucketList{}
		toRead   = numObjects
		listAll  = numObjects == 0
	)
	bckList = &cmn.BucketList{}
	smsg.UUID = ""
	smsg.ContinuationToken = ""
	if len(args) != 0 {
		ctx = args[0]
	}

	// `rem` holds the remaining number of objects to list (that is, unless we are listing
	// the entire bucket). Each iteration lists a page of objects and reduces the `rem`
	// counter accordingly. When the latter gets below page size, we perform the final
	// iteration for the reduced page.
	for pageNum := 1; listAll || toRead > 0; pageNum++ {
		if !listAll {
			smsg.PageSize = toRead
		}
		actMsg := cmn.ActionMsg{Action: cmn.ActList, Value: smsg}
		reqParams.Body = cos.MustMarshal(actMsg)
		page := nextPage

		if pageNum == 1 {
			page = bckList
		} else {
			// Do not try to optimize by reusing allocated page as `Unmarshaler`/`Decoder`
			// will reuse the entry pointers what will result in duplications.
			page.Entries = nil
		}

		// Retry with increasing timeout.
		for i := 0; i < 5; i++ {
			if _, err = doHTTPRequestGetResp(reqParams, page); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					client := *reqParams.BaseParams.Client
					client.Timeout = 2 * client.Timeout
					reqParams.BaseParams.Client = &client
					continue
				}
				return nil, err
			}
			break
		}
		if err != nil {
			return nil, err
		}

		bckList.Flags |= page.Flags
		// The first iteration uses the `bckList` directly so there is no need to append.
		if pageNum > 1 {
			bckList.Entries = append(bckList.Entries, page.Entries...)
			bckList.ContinuationToken = page.ContinuationToken
		}

		if ctx != nil && ctx.mustFire() {
			ctx.info.Count = len(bckList.Entries)
			if page.ContinuationToken == "" {
				ctx.finish()
			}
			ctx.callback(ctx)
		}

		if page.ContinuationToken == "" { // Listed all objects.
			smsg.ContinuationToken = ""
			break
		}

		toRead = uint(cos.Max(int(toRead)-len(page.Entries), 0))
		cos.Assert(page.UUID != "")
		smsg.UUID = page.UUID
		smsg.ContinuationToken = page.ContinuationToken
	}

	return bckList, err
}

// ListObjectsPage returns the first page of bucket objects.
// On success the function updates `smsg.ContinuationToken`, so a client can reuse
// the message to fetch the next page.
func ListObjectsPage(baseParams BaseParams, bck cmn.Bck, smsg *cmn.SelectMsg) (*cmn.BucketList, error) {
	baseParams.Method = http.MethodGet
	if smsg == nil {
		smsg = &cmn.SelectMsg{}
	}

	var (
		actMsg    = cmn.ActionMsg{Action: cmn.ActList, Value: smsg}
		reqParams = ReqParams{
			BaseParams: baseParams,
			Path:       cmn.URLPathBuckets.Join(bck.Name),
			Header:     http.Header{cmn.HdrAccept: []string{cmn.ContentMsgPack}},
			Query:      cmn.AddBckToQuery(url.Values{}, bck),
			Body:       cos.MustMarshal(actMsg),
		}
	)

	// NOTE: No need to preallocate bucket entries slice, we use msgpack so it will do it for us!
	page := &cmn.BucketList{}
	if _, err := doHTTPRequestGetResp(reqParams, page); err != nil {
		return nil, err
	}
	smsg.UUID = page.UUID
	smsg.ContinuationToken = page.ContinuationToken
	return page, nil
}

// TODO: remove this function after introducing mechanism detecting bucket changes.
func ListObjectsInvalidateCache(params BaseParams, bck cmn.Bck) error {
	params.Method = http.MethodPost
	var (
		path = cmn.URLPathBuckets.Join(bck.Name)
		q    = url.Values{}
	)
	return DoHTTPRequest(ReqParams{
		Query:      cmn.AddBckToQuery(q, bck),
		BaseParams: params,
		Path:       path,
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActInvalListCache}),
	})
}

func ECEncodeBucket(baseParams BaseParams, bck cmn.Bck, data, parity int) (xactID string, err error) {
	baseParams.Method = http.MethodPost
	// Without `string` conversion it makes base64 from []byte in `Body`.
	ecConf := string(cos.MustMarshal(&cmn.ECConfToUpdate{
		DataSlices:   &data,
		ParitySlices: &parity,
		Enabled:      Bool(true),
	}))
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.URLPathBuckets.Join(bck.Name),
		Body:       cos.MustMarshal(cmn.ActionMsg{Action: cmn.ActECEncode, Value: ecConf}),
		Query:      cmn.AddBckToQuery(nil, bck),
	}, &xactID)
	return
}

func NewProgressContext(cb ProgressCallback, after time.Duration) *ProgressContext {
	ctx := &ProgressContext{
		info:      ProgressInfo{Count: -1, Total: -1, Percent: -1.0},
		startTime: time.Now(),
		callback:  cb,
	}
	if after != 0 {
		ctx.callAfter = ctx.startTime.Add(after)
	}
	return ctx
}

func (ctx *ProgressContext) finish() {
	ctx.info.Percent = 100.0
	if ctx.info.Total > 0 {
		ctx.info.Count = ctx.info.Total
	}
}

func (ctx *ProgressContext) IsFinished() bool {
	return ctx.info.Percent >= 100.0 ||
		(ctx.info.Total != 0 && ctx.info.Total == ctx.info.Count)
}

func (ctx *ProgressContext) Elapsed() time.Duration {
	return time.Since(ctx.startTime)
}

func (ctx *ProgressContext) mustFire() bool {
	return ctx.callAfter.IsZero() ||
		ctx.callAfter.Before(time.Now())
}

func (ctx *ProgressContext) Info() ProgressInfo {
	return ctx.info
}
