/*
 * Copyright (c) 2017, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/golang/glog"
)

const (
	fslash           = "/"
	s3skipTokenToKey = 3
)

// TODO Fillin AWS specific initialization
type awsif struct {
}

// TODO Fillin GCP specific initialization
type gcpif struct {
}

type cinterface interface {
	listbucket(http.ResponseWriter, string)
	getobj(http.ResponseWriter, string, string, string)
}

// Function for handling request  on specific port
func httphdlr(w http.ResponseWriter, r *http.Request) {
	if glog.V(1) {
		glog.Infof("HTTP request from %s: %s %q", r.RemoteAddr, r.Method, r.URL)
	}

	if ctx.isproxy {
		proxyhdlr(w, r)
	} else {
		servhdlr(w, r)
	}
}

// Servhdlr function serves request coming to listening port of DFC's Storage Server.
// It supports GET method only and return 405 error for non supported Methods.
// This function checks wheather key exists locally or not. If key does not exist locally
// it prepares session and download objects from S3 to path on local host.
func servhdlr(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		atomic.AddInt64(&stats.numget, 1)
		cnt := strings.Count(html.EscapeString(r.URL.Path), fslash)
		s := strings.Split(html.EscapeString(r.URL.Path), fslash)
		bktname := s[1]
		if cnt == 1 {
			// Get with only bucket name will imply getting list of objects from bucket.
			ctx.httprun.cloudobj.listbucket(w, bktname)

		} else {
			// Expecting /<bucketname>/keypath
			s := strings.SplitN(html.EscapeString(r.URL.Path), fslash, s3skipTokenToKey)
			keyname := s[2]
			mpath := doHashfindMountPath(bktname + fslash + keyname)
			fname := mpath + fslash + bktname + fslash + keyname
			// Check wheather filename exists in local directory or not
			_, err := os.Stat(fname)
			if os.IsNotExist(err) {
				atomic.AddInt64(&stats.numnotcached, 1)
				glog.Infof("Bucket %s key %s fqn %q is not cached", bktname, keyname, fname)
				ctx.httprun.cloudobj.getobj(w, mpath, bktname, keyname)
			} else if glog.V(2) {
				glog.Infof("Bucket %s key %s fqn %q *is* cached", bktname, keyname, fname)
			}
			file, err := os.Open(fname)
			if err != nil {
				glog.Errorf("Failed to open file %q, err: %v", fname, err)
				checksetmounterror(fname)
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				defer file.Close()

				// TODO: optimize. Currently the file gets downloaded and stored locally
				//       _prior_ to sending http response back to the requesting client
				_, err := io.Copy(w, file)
				if err != nil {
					glog.Errorf("Failed to copy data to http response for fname %q, err: %v", fname, err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
				} else {
					glog.Infof("Copied %q to http response\n", fname)
				}
			}
		}
	case "POST":
	case "PUT":
	case "DELETE":
	default:
		glog.Errorf("Invalid request from %s: %s %q", r.RemoteAddr, r.Method, r.URL)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed)+": "+r.Method,
			http.StatusMethodNotAllowed)

	}
	glog.Flush()
}

//===========================================================================
//
// http runner
//
//===========================================================================
type httprunner struct {
	listener  net.Listener    // http listener
	fschkchan chan bool       // to stop checkfs timer
	httprqwg  *sync.WaitGroup // to complete pending http
	cloudobj  cinterface      // Interface for multiple cloud
}

// start http runner
func (r *httprunner) run() error {
	var err error
	// server must register with the proxy
	if !ctx.isproxy {
		// Chanel for stopping filesystem check timer.
		r.fschkchan = make(chan bool)
		//
		// FIXME: UNREGISTER is totally missing
		//
		err = registerwithproxy()
		if err != nil {
			glog.Errorf("Failed to parse mounts, err: %v", err)
			return err
		}
		// Local mount points have precedence over cachePath settings.
		ctx.mntpath, err = parseProcMounts(procMountsPath)
		if err != nil {
			glog.Errorf("Failed to register with proxy, err: %v", err)
			return err
		}
		if len(ctx.mntpath) == 0 {
			glog.Infof("Warning: configuring %d mount points for testing purposes", ctx.config.Cache.CachePathCount)

			// Use CachePath from config file if set.
			if ctx.config.Cache.CachePath == "" || ctx.config.Cache.CachePathCount < 1 {
				errstr := fmt.Sprintf("Invalid configuration: CachePath %q or CachePathCount %d",
					ctx.config.Cache.CachePath, ctx.config.Cache.CachePathCount)
				glog.Error(errstr)
				err := errors.New(errstr)
				return err
			}
			ctx.mntpath = populateCachepathMounts()
		} else {
			glog.Infof("Found %d mount points", len(ctx.mntpath))
		}
		// Start FScheck thread
		go fsCheckTimer(r.fschkchan)
	}
	httpmux := http.NewServeMux()
	httpmux.HandleFunc("/", httphdlr)
	portstring := ":" + ctx.config.Listen.Port

	r.listener, err = net.Listen("tcp", portstring)
	if err != nil {
		glog.Errorf("Failed to start listening on port %s, err: %v", portstring, err)
		return err
	}
	// Check and Instantiate Cloud Provider object
	if ctx.config.CloudProvider == amazoncloud {
		// TODO do AWS initialization including sessions.
		obj := awsif{}
		r.cloudobj = &obj

	}
	if ctx.config.CloudProvider == googlecloud {
		obj := gcpif{}
		r.cloudobj = &obj
	}
	if r.cloudobj == nil {
		errstr := fmt.Sprintf("Failed to initialize cloud object provider, provider : %s", ctx.config.CloudProvider)
		glog.Error(errstr)
		err := errors.New(errstr)
		return err

	}
	r.httprqwg = &sync.WaitGroup{}
	ctx.httprun = r
	return http.Serve(r.listener, httpmux)

}

// stop gracefully
func (r *httprunner) stop(err error) {
	if r.listener == nil {
		return
	}
	glog.Infof("Stopping httprunner, err: %v", err)

	// stop listening
	r.listener.Close()

	// wait for the completion of pending requests
	r.httprqwg.Wait()
	if !ctx.isproxy {
		close(r.fschkchan)
	}
}
