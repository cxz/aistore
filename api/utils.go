// Package api provides RESTful API to AIS object storage
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
	"github.com/tinylib/msgp/msgp"
)

type (
	BaseParams struct {
		Client *http.Client
		URL    string
		Method string
		Token  string
	}
	// ReqParams is used in constructing client-side API requests to the AIStore.
	// Stores Query and Headers for providing arguments that are not used commonly in API requests
	ReqParams struct {
		BaseParams BaseParams
		Path       string
		Body       []byte
		Query      url.Values
		Header     http.Header

		// Authentication
		User     string
		Password string

		// Determines if the response should be validated with the checksum
		Validate bool
	}
	wrappedResp struct {
		*http.Response
		n          int64  // number bytes read from `resp.Body`
		cksumValue string // checksum value of the response
	}
)

// HTTPStatus returns HTTP status or (-1) for non-HTTP error
func HTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	herr, ok := err.(*cmn.HTTPError)
	if !ok {
		return -1 // invalid
	}
	return herr.Status
}

// DoHTTPRequest sends one HTTP request and decodes the `v` structure
// (if provided) from `resp.Body`.
func DoHTTPRequest(reqParams ReqParams, vs ...interface{}) error {
	var v interface{}
	if len(vs) > 0 {
		v = vs[0]
	}
	_, err := doHTTPRequestGetResp(reqParams, v)
	return err
}

// doHTTPRequestGetResp sends one HTTP request, decodes the `v` structure
// (if provided) from `resp.Body` and returns the whole response.
func doHTTPRequestGetResp(reqParams ReqParams, v interface{}) (*wrappedResp, error) {
	var (
		reqBody io.Reader
	)
	if reqParams.Body != nil {
		reqBody = bytes.NewBuffer(reqParams.Body)
	}

	urlPath := reqParams.BaseParams.URL + reqParams.Path
	req, err := http.NewRequest(reqParams.BaseParams.Method, urlPath, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request, err: %v", err)
	}
	setRequestOptParams(req, reqParams)
	setAuthToken(req, reqParams.BaseParams)

	resp, err := reqParams.BaseParams.Client.Do(req) // nolint:bodyclose // it's closed later
	if err != nil {
		sleep := httpRetrySleep
		if cmn.IsErrConnectionReset(err) || cmn.IsErrConnectionRefused(err) {
			for i := 0; i < httpMaxRetries && err != nil; i++ {
				time.Sleep(sleep)
				resp, err = reqParams.BaseParams.Client.Do(req) // nolint:bodyclose // it's closed later
				sleep += sleep / 2
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to %s, err: %v", reqParams.BaseParams.Method, err)
	}
	defer resp.Body.Close()
	return readResp(reqParams, resp, v)
}

func readResp(reqParams ReqParams, resp *http.Response, v interface{}) (*wrappedResp, error) {
	defer cmn.DrainReader(resp.Body)

	if resp.StatusCode >= http.StatusBadRequest {
		var httpErr *cmn.HTTPError
		if reqParams.BaseParams.Method != http.MethodHead && resp.StatusCode != http.StatusServiceUnavailable {
			if jsonErr := jsoniter.NewDecoder(resp.Body).Decode(&httpErr); jsonErr == nil {
				return nil, httpErr
			}
		}
		msg, _ := ioutil.ReadAll(resp.Body)
		strMsg := string(msg)

		if resp.StatusCode == http.StatusServiceUnavailable && strMsg == "" {
			strMsg = fmt.Sprintf("[%s]: starting up, please wait a little and retry",
				http.StatusText(http.StatusServiceUnavailable))
		}
		// HEAD request does not return the body - create http error
		// 503 is also to be preserved
		httpErr = &cmn.HTTPError{
			Status:  resp.StatusCode,
			Method:  reqParams.BaseParams.Method,
			URLPath: reqParams.Path,
			Message: strMsg,
		}
		return nil, httpErr
	}
	wresp := &wrappedResp{Response: resp}
	if v == nil {
		return wresp, nil
	}
	if w, ok := v.(io.Writer); ok {
		if !reqParams.Validate {
			n, err := io.Copy(w, resp.Body)
			if err != nil {
				return nil, err
			}
			wresp.n = n
		} else {
			hdrCksumType := resp.Header.Get(cmn.HeaderObjCksumType)
			// TODO: use MMSA
			n, cksum, err := cmn.CopyAndChecksum(w, resp.Body, nil, hdrCksumType)
			if err != nil {
				return nil, err
			}
			wresp.n = n
			if cksum != nil {
				wresp.cksumValue = cksum.Value()
			}
		}
	} else {
		var err error
		switch t := v.(type) {
		case *string:
			// In some places like dSort, the response is just a string (id).
			var b []byte
			b, err = ioutil.ReadAll(resp.Body)
			*t = string(b)
		default:
			if resp.StatusCode == http.StatusOK {
				if resp.Header.Get(cmn.HeaderContentType) == cmn.ContentMsgPack {
					r := msgp.NewReaderSize(resp.Body, 10*cmn.KiB)
					err = v.(msgp.Decodable).DecodeMsg(r)
				} else {
					err = jsoniter.NewDecoder(resp.Body).Decode(v)
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read response, err: %v", err)
		}
	}
	return wresp, nil
}

// Given an existing HTTP Request and optional API parameters, setRequestOptParams
// sets the optional fields of the request if provided
func setRequestOptParams(req *http.Request, reqParams ReqParams) {
	if len(reqParams.Query) != 0 {
		req.URL.RawQuery = reqParams.Query.Encode()
	}
	if reqParams.Header != nil {
		req.Header = reqParams.Header
	}
	if reqParams.User != "" && reqParams.Password != "" {
		req.SetBasicAuth(reqParams.User, reqParams.Password)
	}
}

func getObjectOptParams(options GetObjectInput) (w io.Writer, q url.Values, hdr http.Header) {
	w = ioutil.Discard
	if options.Writer != nil {
		w = options.Writer
	}
	if len(options.Query) != 0 {
		q = options.Query
	}
	if len(options.Header) != 0 {
		hdr = options.Header
	}
	return
}

func setAuthToken(r *http.Request, baseParams BaseParams) {
	if baseParams.Token != "" {
		r.Header.Set(cmn.HeaderAuthorization, cmn.MakeHeaderAuthnToken(baseParams.Token))
	}
}

func GetWhatRawQuery(getWhat, getProps string) string {
	q := url.Values{}
	q.Add(cmn.URLParamWhat, getWhat)
	if getProps != "" {
		q.Add(cmn.URLParamProps, getProps)
	}
	return q.Encode()
}
