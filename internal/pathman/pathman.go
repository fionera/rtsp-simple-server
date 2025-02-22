package pathman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aler9/gortsplib/pkg/base"
	"github.com/aler9/gortsplib/pkg/headers"
	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/path"
	"github.com/aler9/rtsp-simple-server/internal/readpublisher"
	"github.com/aler9/rtsp-simple-server/internal/stats"
)

func ipEqualOrInRange(ip net.IP, ips []interface{}) bool {
	for _, item := range ips {
		switch titem := item.(type) {
		case net.IP:
			if titem.Equal(ip) {
				return true
			}

		case *net.IPNet:
			if titem.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// Parent is implemented by program.
type Parent interface {
	Log(logger.Level, string, ...interface{})
}

// PathManager is a path.Path manager.
type PathManager struct {
	rtspAddress     string
	readTimeout     time.Duration
	writeTimeout    time.Duration
	readBufferCount int
	readBufferSize  int
	authMethods     []headers.AuthMethod
	pathConfs       map[string]*conf.PathConf
	stats           *stats.Stats
	parent          Parent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup
	paths     map[string]*path.Path

	// in
	confReload  chan map[string]*conf.PathConf
	pathClose   chan *path.Path
	rpDescribe  chan readpublisher.DescribeReq
	rpSetupPlay chan readpublisher.SetupPlayReq
	rpAnnounce  chan readpublisher.AnnounceReq
}

// New allocates a PathManager.
func New(
	ctxParent context.Context,
	rtspAddress string,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	readBufferCount int,
	readBufferSize int,
	authMethods []headers.AuthMethod,
	pathConfs map[string]*conf.PathConf,
	stats *stats.Stats,
	parent Parent) *PathManager {
	ctx, ctxCancel := context.WithCancel(ctxParent)

	pm := &PathManager{
		rtspAddress:     rtspAddress,
		readTimeout:     readTimeout,
		writeTimeout:    writeTimeout,
		readBufferCount: readBufferCount,
		readBufferSize:  readBufferSize,
		authMethods:     authMethods,
		pathConfs:       pathConfs,
		stats:           stats,
		parent:          parent,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
		paths:           make(map[string]*path.Path),
		confReload:      make(chan map[string]*conf.PathConf),
		pathClose:       make(chan *path.Path),
		rpDescribe:      make(chan readpublisher.DescribeReq),
		rpSetupPlay:     make(chan readpublisher.SetupPlayReq),
		rpAnnounce:      make(chan readpublisher.AnnounceReq),
	}

	pm.createPaths()

	pm.wg.Add(1)
	go pm.run()

	return pm
}

// Close closes a PathManager.
func (pm *PathManager) Close() {
	pm.ctxCancel()
	pm.wg.Wait()
}

// Log is the main logging function.
func (pm *PathManager) Log(level logger.Level, format string, args ...interface{}) {
	pm.parent.Log(level, format, args...)
}

func (pm *PathManager) run() {
	defer pm.wg.Done()

outer:
	for {
		select {
		case pathConfs := <-pm.confReload:
			// remove confs
			for pathName := range pm.pathConfs {
				if _, ok := pathConfs[pathName]; !ok {
					delete(pm.pathConfs, pathName)
				}
			}

			// update confs
			for pathName, oldConf := range pm.pathConfs {
				if !oldConf.Equal(pathConfs[pathName]) {
					pm.pathConfs[pathName] = pathConfs[pathName]
				}
			}

			// add confs
			for pathName, pathConf := range pathConfs {
				if _, ok := pm.pathConfs[pathName]; !ok {
					pm.pathConfs[pathName] = pathConf
				}
			}

			// remove paths associated with a conf which doesn't exist anymore
			// or has changed
			for source, pa := range pm.paths {
				if pathConf, ok := pm.pathConfs[pa.ConfName()]; !ok || pathConf != pa.Conf() {
					delete(pm.paths, source)
					pa.Close()
				}
			}

			// add paths
			pm.createPaths()

		case pa := <-pm.pathClose:
			if pmpa, ok := pm.paths[pa.Conf().Source]; !ok || pmpa != pa {
				continue
			}
			delete(pm.paths, pa.Conf().Source)
			pa.Close()

		case req := <-pm.rpDescribe:
			pathName, pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- readpublisher.DescribeRes{Err: err}
				continue
			}

			action, err := pm.DoAuthRequest(pathConf, PlayRequestPayload{
				RemoteAddr: req.RemoteAddr,
				LocalAddr:  req.LocalAddr,
				Path:       req.PathName,
			})
			if err != nil {
				req.Res <- readpublisher.DescribeRes{Err: err}
				continue
			}

			if action.Close {
				req.Res <- readpublisher.DescribeRes{Err: fmt.Errorf("not allowed")}
				continue
			}

			if action.Target != "" {
				p := *pathConf
				pathConf = &p
				pathConf.Source = action.Target
			}

			err = pm.authenticate(
				req.IP,
				req.ValidateCredentials,
				req.PathName,
				pathConf.ReadIPsParsed,
				pathConf.ReadUser,
				pathConf.ReadPass,
			)
			if err != nil {
				req.Res <- readpublisher.DescribeRes{Err: err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[pathConf.Source]; !ok {
				pm.paths[pathConf.Source] = pm.createPath(pathName, pathConf, req.PathName)
			}

			pm.paths[pathConf.Source].OnPathManDescribe(req)

		case req := <-pm.rpSetupPlay:
			pathName, pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- readpublisher.SetupPlayRes{Err: err}
				continue
			}

			action, err := pm.DoAuthRequest(pathConf, PlayRequestPayload{
				RemoteAddr: req.RemoteAddr,
				LocalAddr:  req.LocalAddr,
				Path:       req.PathName,
			})
			if err != nil {
				req.Res <- readpublisher.SetupPlayRes{Err: err}
				continue
			}

			if action.Close {
				req.Res <- readpublisher.SetupPlayRes{Err: fmt.Errorf("not allowed")}
				continue
			}

			if action.Target != "" {
				p := *pathConf
				pathConf = &p
				pathConf.Source = action.Target
			}

			err = pm.authenticate(
				req.IP,
				req.ValidateCredentials,
				req.PathName,
				pathConf.ReadIPsParsed,
				pathConf.ReadUser,
				pathConf.ReadPass,
			)
			if err != nil {
				req.Res <- readpublisher.SetupPlayRes{Err: err}
				continue
			}

			if _, ok := pm.paths[pathConf.Source]; !ok {
				pm.paths[pathConf.Source] = pm.createPath(pathName, pathConf, req.PathName)
			}

			pm.paths[pathConf.Source].OnPathManSetupPlay(req)

		case req := <-pm.rpAnnounce:
			pathName, pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- readpublisher.AnnounceRes{Err: err}
				continue
			}

			err = pm.authenticate(
				req.IP,
				req.ValidateCredentials,
				req.PathName,
				pathConf.PublishIPsParsed,
				pathConf.PublishUser,
				pathConf.PublishPass,
			)
			if err != nil {
				req.Res <- readpublisher.AnnounceRes{Err: err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[pathConf.Source]; !ok {
				pm.createPath(pathName, pathConf, req.PathName)
			}

			pm.paths[pathConf.Source].OnPathManAnnounce(req)

		case <-pm.ctx.Done():
			break outer
		}
	}

	pm.ctxCancel()
}

func (pm *PathManager) createPath(confName string, conf *conf.PathConf, name string) *path.Path {
	return path.New(
		pm.ctx,
		pm.rtspAddress,
		pm.readTimeout,
		pm.writeTimeout,
		pm.readBufferCount,
		pm.readBufferSize,
		confName,
		conf,
		name,
		&pm.wg,
		pm.stats,
		pm)
}

func (pm *PathManager) createPaths() {
	for pathName, pathConf := range pm.pathConfs {
		if _, ok := pm.paths[pathConf.Source]; !ok && pathConf.Regexp == nil {
			pm.createPath(pathName, pathConf, pathName)
		}
	}
}

func (pm *PathManager) findPathConf(name string) (string, *conf.PathConf, error) {
	err := conf.CheckPathName(name)
	if err != nil {
		return "", nil, fmt.Errorf("invalid path name: %s (%s)", err, name)
	}

	// normal path
	if pathConf, ok := pm.pathConfs[name]; ok {
		return name, pathConf.GetInstance(name), nil
	}

	// regular expression path
	for pathName, pathConf := range pm.pathConfs {
		if pathConf.Regexp != nil && pathConf.Regexp.MatchString(name) {
			return pathName, pathConf.GetInstance(name), nil
		}
	}

	return "", nil, fmt.Errorf("unable to find a valid configuration for path '%s'", name)
}

// OnProgramConfReload is called by program.
func (pm *PathManager) OnProgramConfReload(pathConfs map[string]*conf.PathConf) {
	select {
	case pm.confReload <- pathConfs:
	case <-pm.ctx.Done():
	}
}

// OnPathClose is called by path.Path.
func (pm *PathManager) OnPathClose(pa *path.Path) {
	select {
	case pm.pathClose <- pa:
	case <-pm.ctx.Done():
	}
}

// OnReadPublisherDescribe is called by a ReadPublisher.
func (pm *PathManager) OnReadPublisherDescribe(req readpublisher.DescribeReq) {
	select {
	case pm.rpDescribe <- req:
	case <-pm.ctx.Done():
		req.Res <- readpublisher.DescribeRes{Err: fmt.Errorf("terminated")}
	}
}

// OnReadPublisherAnnounce is called by a ReadPublisher.
func (pm *PathManager) OnReadPublisherAnnounce(req readpublisher.AnnounceReq) {
	select {
	case pm.rpAnnounce <- req:
	case <-pm.ctx.Done():
		req.Res <- readpublisher.AnnounceRes{Err: fmt.Errorf("terminated")}
	}
}

// OnReadPublisherSetupPlay is called by a ReadPublisher.
func (pm *PathManager) OnReadPublisherSetupPlay(req readpublisher.SetupPlayReq) {
	select {
	case pm.rpSetupPlay <- req:
	case <-pm.ctx.Done():
		req.Res <- readpublisher.SetupPlayRes{Err: fmt.Errorf("terminated")}
	}
}

func (pm *PathManager) authenticate(
	ip net.IP,
	validateCredentials func(authMethods []headers.AuthMethod, pathUser string, pathPass string) error,
	pathName string,
	pathIPs []interface{},
	pathUser string,
	pathPass string,
) error {
	// validate ip
	if pathIPs != nil && ip != nil {
		if !ipEqualOrInRange(ip, pathIPs) {
			return readpublisher.ErrAuthCritical{
				Message: fmt.Sprintf("IP '%s' not allowed", ip),
				Response: &base.Response{
					StatusCode: base.StatusUnauthorized,
				},
			}
		}
	}

	// validate user
	if pathUser != "" && validateCredentials != nil {
		err := validateCredentials(pm.authMethods, pathUser, pathPass)
		if err != nil {
			return err
		}
	}

	return nil
}

type PlayRequestPayload struct {
	RemoteAddr string `json:"remote_addr"`
	LocalAddr  string `json:"local_addr"`
	Path       string `json:"path"`
}

type PlayRequestAction struct {
	Close  bool
	Target string
}

func (pm *PathManager) DoAuthRequest(pathConf *conf.PathConf, p PlayRequestPayload) (*PlayRequestAction, error) {
	var a PlayRequestAction

	if pathConf.HTTPCallback == "" {
		return &a, nil
	}

	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}

	c := http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := c.Post(pathConf.HTTPCallback, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
	case resp.StatusCode >= 300 && resp.StatusCode <= 399:
		a.Target = resp.Header.Get("Location")
		if a.Target == "" {
			return nil, fmt.Errorf("invalid location header in redirect response. closing connection")
		}
	case resp.StatusCode >= 400 && resp.StatusCode <= 499:
		a.Close = true
	default:
		return nil, fmt.Errorf("invalid redirect response. closing connection")
	}

	return &a, nil
}
