package pathman

import (
	"fmt"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/base"
	"github.com/aler9/gortsplib/headers"

	"github.com/aler9/rtsp-simple-server/client"
	"github.com/aler9/rtsp-simple-server/conf"
	"github.com/aler9/rtsp-simple-server/path"
	"github.com/aler9/rtsp-simple-server/serverudp"
	"github.com/aler9/rtsp-simple-server/stats"
)

type Parent interface {
	Log(string, ...interface{})
}

type PathManager struct {
	stats         *stats.Stats
	serverUdpRtp  *serverudp.Server
	serverUdpRtcp *serverudp.Server
	readTimeout   time.Duration
	writeTimeout  time.Duration
	authMethods   []headers.AuthMethod
	confPaths     map[string]*conf.PathConf
	parent        Parent

	paths map[string]*path.Path
	wg    sync.WaitGroup

	// in
	pathClose       chan *path.Path
	clientDescribe  chan path.ClientDescribeReq
	clientAnnounce  chan path.ClientAnnounceReq
	clientSetupPlay chan path.ClientSetupPlayReq
	terminate       chan struct{}

	// out
	clientClose chan *client.Client
	done        chan struct{}
}

func New(stats *stats.Stats,
	serverUdpRtp *serverudp.Server,
	serverUdpRtcp *serverudp.Server,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	authMethods []headers.AuthMethod,
	confPaths map[string]*conf.PathConf,
	parent Parent) *PathManager {

	pm := &PathManager{
		stats:           stats,
		serverUdpRtp:    serverUdpRtp,
		serverUdpRtcp:   serverUdpRtcp,
		readTimeout:     readTimeout,
		writeTimeout:    writeTimeout,
		authMethods:     authMethods,
		confPaths:       confPaths,
		parent:          parent,
		paths:           make(map[string]*path.Path),
		pathClose:       make(chan *path.Path),
		clientDescribe:  make(chan path.ClientDescribeReq),
		clientAnnounce:  make(chan path.ClientAnnounceReq),
		clientSetupPlay: make(chan path.ClientSetupPlayReq),
		terminate:       make(chan struct{}),
		clientClose:     make(chan *client.Client),
		done:            make(chan struct{}),
	}

	for name, pathConf := range confPaths {
		if pathConf.Regexp == nil {
			pa := path.New(&pm.wg, pm.stats, pm.serverUdpRtp, pm.serverUdpRtcp,
				pm.readTimeout, pm.writeTimeout, name, pathConf, pm)
			pm.paths[name] = pa
		}
	}

	go pm.run()
	return pm
}

func (pm *PathManager) Close() {
	go func() {
		for range pm.clientClose {
		}
	}()
	close(pm.terminate)
	<-pm.done
}

func (pm *PathManager) Log(format string, args ...interface{}) {
	pm.parent.Log(format, args...)
}

func (pm *PathManager) run() {
	defer close(pm.done)

outer:
	for {
		select {
		case pa := <-pm.pathClose:
			delete(pm.paths, pa.Name())
			pa.Close()

		case req := <-pm.clientDescribe:
			pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- path.ClientDescribeRes{nil, err}
				continue
			}

			err = req.Client.Authenticate(pm.authMethods, pathConf.ReadIpsParsed,
				pathConf.ReadUser, pathConf.ReadPass, req.Req)
			if err != nil {
				req.Res <- path.ClientDescribeRes{nil, err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.PathName]; !ok {
				pa := path.New(&pm.wg, pm.stats, pm.serverUdpRtp, pm.serverUdpRtcp,
					pm.readTimeout, pm.writeTimeout, req.PathName, pathConf, pm)
				pm.paths[req.PathName] = pa
			}

			pm.paths[req.PathName].OnPathManDescribe(req)

		case req := <-pm.clientAnnounce:
			pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- path.ClientAnnounceRes{nil, err}
				continue
			}

			err = req.Client.Authenticate(pm.authMethods,
				pathConf.PublishIpsParsed, pathConf.PublishUser, pathConf.PublishPass, req.Req)
			if err != nil {
				req.Res <- path.ClientAnnounceRes{nil, err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.PathName]; !ok {
				pa := path.New(&pm.wg, pm.stats, pm.serverUdpRtp, pm.serverUdpRtcp,
					pm.readTimeout, pm.writeTimeout, req.PathName, pathConf, pm)
				pm.paths[req.PathName] = pa
			}

			pm.paths[req.PathName].OnPathManAnnounce(req)

		case req := <-pm.clientSetupPlay:
			if _, ok := pm.paths[req.PathName]; !ok {
				req.Res <- path.ClientSetupPlayRes{nil, fmt.Errorf("no one is publishing on path '%s'", req.PathName)}
				continue
			}

			pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- path.ClientSetupPlayRes{nil, err}
				continue
			}

			err = req.Client.Authenticate(pm.authMethods,
				pathConf.ReadIpsParsed, pathConf.ReadUser, pathConf.ReadPass, req.Req)
			if err != nil {
				req.Res <- path.ClientSetupPlayRes{nil, err}
				continue
			}

			pm.paths[req.PathName].OnPathManSetupPlay(req)

		case <-pm.terminate:
			break outer
		}
	}

	go func() {
		for {
			select {
			case _, ok := <-pm.pathClose:
				if !ok {
					return
				}

			case req := <-pm.clientDescribe:
				req.Res <- path.ClientDescribeRes{nil, fmt.Errorf("terminated")}

			case req := <-pm.clientAnnounce:
				req.Res <- path.ClientAnnounceRes{nil, fmt.Errorf("terminated")}

			case req := <-pm.clientSetupPlay:
				req.Res <- path.ClientSetupPlayRes{nil, fmt.Errorf("terminated")}
			}
		}
	}()

	for _, pa := range pm.paths {
		pa.Close()
	}
	pm.wg.Wait()

	close(pm.clientClose)
	close(pm.pathClose)
	close(pm.clientDescribe)
	close(pm.clientAnnounce)
	close(pm.clientSetupPlay)
}

func (pm *PathManager) findPathConf(name string) (*conf.PathConf, error) {
	err := conf.CheckPathName(name)
	if err != nil {
		return nil, fmt.Errorf("invalid path name: %s (%s)", err, name)
	}

	// normal path
	if pathConf, ok := pm.confPaths[name]; ok {
		return pathConf, nil
	}

	// regular expression path
	for _, pathConf := range pm.confPaths {
		if pathConf.Regexp != nil && pathConf.Regexp.MatchString(name) {
			return pathConf, nil
		}
	}

	return nil, fmt.Errorf("unable to find a valid configuration for path '%s'", name)
}

func (pm *PathManager) OnPathClose(pa *path.Path) {
	pm.pathClose <- pa
}

func (pm *PathManager) OnPathClientClose(c *client.Client) {
	pm.clientClose <- c
}

func (pm *PathManager) OnClientDescribe(c *client.Client, pathName string, req *base.Request) (client.Path, error) {
	res := make(chan path.ClientDescribeRes)
	pm.clientDescribe <- path.ClientDescribeReq{res, c, pathName, req}
	re := <-res
	return re.Path, re.Err
}

func (pm *PathManager) OnClientAnnounce(c *client.Client, pathName string, tracks gortsplib.Tracks, req *base.Request) (client.Path, error) {
	res := make(chan path.ClientAnnounceRes)
	pm.clientAnnounce <- path.ClientAnnounceReq{res, c, pathName, tracks, req}
	re := <-res
	return re.Path, re.Err
}

func (pm *PathManager) OnClientSetupPlay(c *client.Client, pathName string, trackId int, req *base.Request) (client.Path, error) {
	res := make(chan path.ClientSetupPlayRes)
	pm.clientSetupPlay <- path.ClientSetupPlayReq{res, c, pathName, trackId, req}
	re := <-res
	return re.Path, re.Err
}

func (pm *PathManager) ClientClose() chan *client.Client {
	return pm.clientClose
}