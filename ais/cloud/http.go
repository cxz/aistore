// Package cloud contains implementation of various cloud providers.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cloud

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/debug"
)

type (
	httpProvider struct {
		t           cluster.Target
		httpClient  *http.Client
		httpsClient *http.Client
	}
)

var (
	_ cluster.CloudProvider = &httpProvider{}
)

func NewHTTP(t cluster.Target, config *cmn.Config) (cluster.CloudProvider, error) {
	hp := &httpProvider{t: t}
	hp.httpClient = cmn.NewClient(cmn.TransportArgs{
		Timeout:         config.Client.TimeoutLong,
		WriteBufferSize: config.Net.HTTP.WriteBufferSize,
		ReadBufferSize:  config.Net.HTTP.ReadBufferSize,
		UseHTTPS:        false,
		SkipVerify:      config.Net.HTTP.SkipVerify,
	})
	hp.httpsClient = cmn.NewClient(cmn.TransportArgs{
		Timeout:         config.Client.TimeoutLong,
		WriteBufferSize: config.Net.HTTP.WriteBufferSize,
		ReadBufferSize:  config.Net.HTTP.ReadBufferSize,
		UseHTTPS:        true,
		SkipVerify:      config.Net.HTTP.SkipVerify,
	})
	return hp, nil
}

func (hp *httpProvider) client(u string) *http.Client {
	if strings.HasPrefix(u, "https") {
		return hp.httpsClient
	}
	return hp.httpClient
}

func (hp *httpProvider) Provider() string  { return cmn.ProviderHTTP }
func (hp *httpProvider) MaxPageSize() uint { return 10000 }

func (hp *httpProvider) ListBuckets(_ context.Context, _ cmn.QueryBcks) (_ cmn.BucketNames, _ error, _ int) {
	debug.Assert(false)
	return
}

func (hp *httpProvider) ListObjects(_ context.Context, _ *cluster.Bck, _ *cmn.SelectMsg) (_ *cmn.BucketList, _ error, _ int) {
	debug.Assert(false)
	return
}

func (hp *httpProvider) PutObj(_ context.Context, _ io.Reader, _ *cluster.LOM) (string, error, int) {
	return "", fmt.Errorf("%q provider doesn't support creating new objects", hp.Provider()), http.StatusBadRequest
}

func (hp *httpProvider) DeleteObj(_ context.Context, _ *cluster.LOM) (error, int) {
	return fmt.Errorf("%q provider doesn't support deleting object", hp.Provider()), http.StatusBadRequest
}

func getOriginalURL(ctx context.Context, bck *cluster.Bck, objName string) (string, error) {
	origURL, ok := ctx.Value(cmn.CtxOriginalURL).(string)
	if !ok || origURL == "" {
		if bck.Props == nil {
			return "", fmt.Errorf("failed to HEAD (%s): original_url is empty", bck.Bck)
		}
		origURL = bck.Props.Extra.OrigURLBck
		debug.Assert(origURL != "")
		if objName != "" {
			origURL = cmn.JoinPath(bck.Props.Extra.OrigURLBck, objName) // see `cmn.URL2BckObj`
		}
	}
	return origURL, nil
}

func (hp *httpProvider) HeadBucket(ctx context.Context, bck *cluster.Bck) (bckProps cmn.SimpleKVs, err error, errCode int) {
	// TODO: we should use `bck.BackendBck()`.

	origURL, err := getOriginalURL(ctx, bck, "")
	if err != nil {
		return nil, err, http.StatusBadRequest
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[head_bucket] original_url: %q", origURL)
	}

	// Contact the original URL - as long as we can make connection we assume it's good.
	resp, err := hp.client(origURL).Head(origURL)
	if err != nil {
		return nil, err, http.StatusBadRequest
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("HEAD(%s) failed, status %d", origURL, resp.StatusCode)
		return nil, err, resp.StatusCode
	}

	// TODO: improve validation - check `content-type` header
	if resp.Header.Get(cmn.HeaderETag) == "" {
		err = fmt.Errorf("invalid resource - missing header %s", cmn.HeaderETag)
		return nil, err, http.StatusBadRequest
	}

	resp.Body.Close()

	bckProps = make(cmn.SimpleKVs)
	bckProps[cmn.HeaderCloudProvider] = cmn.ProviderHTTP
	return
}

func (hp *httpProvider) HeadObj(ctx context.Context, lom *cluster.LOM) (objMeta cmn.SimpleKVs, err error, errCode int) {
	var (
		h   = cmn.CloudHelpers.HTTP
		bck = lom.Bck() // TODO: This should be `cloudBck = lom.Bck().BackendBck()`
	)

	origURL, err := getOriginalURL(ctx, bck, lom.ObjName)
	debug.AssertNoErr(err)

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[head_object] original_url: %q", origURL)
	}

	resp, err := hp.client(origURL).Head(origURL)
	if err != nil {
		return nil, err, http.StatusBadRequest
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error occurred: %v", resp.StatusCode), resp.StatusCode
	}
	objMeta = make(cmn.SimpleKVs, 2)
	objMeta[cmn.HeaderCloudProvider] = cmn.ProviderHTTP
	if resp.ContentLength >= 0 {
		objMeta[cmn.HeaderObjSize] = strconv.FormatInt(resp.ContentLength, 10)
	}
	if v, ok := h.EncodeVersion(resp.Header.Get(cmn.HeaderETag)); ok {
		objMeta[cluster.VersionObjMD] = v
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[head_object] %s", lom)
	}
	return
}

func (hp *httpProvider) GetObj(ctx context.Context, workFQN string, lom *cluster.LOM) (err error, errCode int) {
	var (
		h   = cmn.CloudHelpers.HTTP
		bck = lom.Bck() // TODO: This should be `cloudBck = lom.Bck().BackendBck()`
	)

	origURL, err := getOriginalURL(ctx, bck, lom.ObjName)
	debug.AssertNoErr(err)

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[HTTP CLOUD][GET] original_url: %q", origURL)
	}

	resp, err := hp.client(origURL).Get(origURL)
	if err != nil {
		return err, http.StatusInternalServerError
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error occurred: %v", resp.StatusCode), resp.StatusCode
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[HTTP CLOUD][GET] success, size: %d", resp.ContentLength)
	}

	customMD := cmn.SimpleKVs{
		cluster.SourceObjMD:  cluster.SourceHTTPObjMD,
		cluster.OrigURLObjMD: origURL,
	}
	if v, ok := h.EncodeVersion(resp.Header.Get(cmn.HeaderETag)); ok {
		customMD[cluster.VersionObjMD] = v
	}

	lom.SetCustomMD(customMD)
	setSize(ctx, resp.ContentLength)
	err = hp.t.PutObject(cluster.PutObjectParams{
		LOM:          lom,
		Reader:       wrapReader(ctx, resp.Body),
		WorkFQN:      workFQN,
		RecvType:     cluster.ColdGet,
		WithFinalize: false,
	})
	if err != nil {
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[get_object] %s", lom)
	}
	return
}
