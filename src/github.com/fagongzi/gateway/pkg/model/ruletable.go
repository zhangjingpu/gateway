package model

import (
	"errors"
	"sync"
	"time"

	"github.com/fagongzi/gateway/pkg/conf"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/log"
	"github.com/fagongzi/util/task"
	"github.com/valyala/fasthttp"
)

var (
	// ErrServerExists Server already exist
	ErrServerExists = errors.New("Server already exist")
	// ErrClusterExists Cluster already exist
	ErrClusterExists = errors.New("Cluster already exist")
	// ErrBindExists Bind already exist
	ErrBindExists = errors.New("Bind already exist")
	// ErrAPIExists API already exist
	ErrAPIExists = errors.New("API already exist")
	// ErrRoutingExists Routing already exist
	ErrRoutingExists = errors.New("Routing already exist")
	// ErrServerNotFound Server not found
	ErrServerNotFound = errors.New("Server not found")
	// ErrClusterNotFound Cluster not found
	ErrClusterNotFound = errors.New("Cluster not found")
	// ErrBindNotFound Bind not found
	ErrBindNotFound = errors.New("Bind not found")
	// ErrAPINotFound API not found
	ErrAPINotFound = errors.New("API not found")
	// ErrRoutingNotFound Routing not found
	ErrRoutingNotFound = errors.New("Routing not found")
)

// RouteResult RouteResult
type RouteResult struct {
	API   *API
	Node  *Node
	Svr   *Server
	Err   error
	Code  int
	Res   *fasthttp.Response
	Merge bool
}

// Release release resp
func (result *RouteResult) Release() {
	if nil != result.Res {
		fasthttp.ReleaseResponse(result.Res)
	}
}

// NeedRewrite need rewrite
func (result *RouteResult) NeedRewrite() bool {
	return result.Node != nil && result.Node.Rewrite != ""
}

// GetRewritePath get rewrite path
func (result *RouteResult) GetRewritePath(req *fasthttp.Request) string {
	if nil != result.Node {
		return result.API.getNodeURL(req, result.Node)
	}

	return ""
}

// RouteTable route table
type RouteTable struct {
	rwLock *sync.RWMutex

	cnf *conf.Conf

	clusters map[string]*Cluster
	svrs     map[string]*Server
	mapping  map[string]map[string]*Cluster
	apis     map[string]*API
	routings map[string]*Routing

	store Store

	tw *goetty.HashedTimeWheel

	evtChan        chan *Server
	watchStopCh    chan bool
	watchReceiveCh chan *Evt

	analysiser *Analysis
}

// NewRouteTable create a new RouteTable
func NewRouteTable(cnf *conf.Conf, store Store, taskRunner *task.Runner) *RouteTable {
	tw := goetty.NewHashedTimeWheel(time.Second, 60, 3)
	tw.Start()

	rt := &RouteTable{
		cnf:   cnf,
		tw:    tw,
		store: store,

		analysiser: newAnalysis(taskRunner),

		rwLock: &sync.RWMutex{},

		clusters: make(map[string]*Cluster),
		svrs:     make(map[string]*Server),
		apis:     make(map[string]*API),
		routings: make(map[string]*Routing),
		mapping:  make(map[string]map[string]*Cluster), // serverAddr -> map[clusterName]*Cluster

		evtChan:        make(chan *Server, 1024),
		watchStopCh:    make(chan bool),
		watchReceiveCh: make(chan *Evt),
	}

	go rt.changed()
	return rt
}

// GetServer return server
func (r *RouteTable) GetServer(addr string) *Server {
	return r.svrs[addr]
}

// AddNewRouting add a new route
func (r *RouteTable) AddNewRouting(routing *Routing) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	err := routing.Check()

	if nil != err {
		return err
	}

	_, ok := r.routings[routing.ID]

	if ok {
		return ErrRoutingExists
	}

	r.routings[routing.ID] = routing

	log.Infof("meta: routing <%s> added", routing.Cfg)

	return nil
}

// DeleteRouting delete a route
func (r *RouteTable) DeleteRouting(id string) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	route, ok := r.routings[id]

	if !ok {
		return ErrRoutingNotFound
	}

	delete(r.routings, id)

	log.Infof("meta: routing <%s> deleted", route.Cfg)

	return nil
}

// AddNewAPI add a new API
func (r *RouteTable) AddNewAPI(api *API) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	_, ok := r.apis[api.URL]

	if ok {
		return ErrAPIExists
	}

	api.Parse()

	r.apis[getAPIKey(api.URL, api.Method)] = api

	log.Infof("meta: api <%s-%s> added", api.Method, api.URL)

	return nil
}

// UpdateAPI update API
func (r *RouteTable) UpdateAPI(api *API) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	if _, ok := r.apis[getAPIKey(api.URL, api.Method)]; !ok {
		return ErrAPINotFound
	}

	r.apis[getAPIKey(api.URL, api.Method)] = api
	api.Parse()

	log.Infof("meta: api <%s-%s> updated", api.Method, api.URL)

	return nil
}

// DeleteAPI delete a api using url
func (r *RouteTable) DeleteAPI(url, method string) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	_, ok := r.apis[getAPIKey(url, method)]

	if !ok {
		return ErrAPINotFound
	}

	delete(r.apis, getAPIKey(url, method))

	log.Infof("meta: api <%s-%s> deleted", method, url)

	return nil
}

// UpdateServer update server
func (r *RouteTable) UpdateServer(svr *Server) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	old, ok := r.svrs[svr.Addr]

	if !ok {
		return ErrServerNotFound
	}

	r.GetAnalysis().AddRecentCount(svr.Addr, svr.OpenToCloseCollectSeconds)
	r.GetAnalysis().AddRecentCount(svr.Addr, svr.OpenToCloseCollectSeconds)

	old.updateFrom(svr)

	log.Infof("meta: server <%s> updated", svr.Addr)

	return nil
}

// DeleteServer delete a server
func (r *RouteTable) DeleteServer(serverAddr string) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	svr, ok := r.svrs[serverAddr]

	if !ok {
		return ErrServerNotFound
	}

	delete(r.svrs, serverAddr)

	// TODO: delete apis

	svr.stopCheck()
	r.removeFromCheck(svr)

	binded, _ := r.mapping[svr.Addr]
	delete(r.mapping, svr.Addr)
	log.Infof("meta: bind <%s> stored all removed", svr.Addr)

	for _, cluster := range binded {
		cluster.unbind(svr)
	}

	log.Infof("meta: server <%s> deleted", svr.Addr)

	return nil
}

// AddNewServer add a new server
func (r *RouteTable) AddNewServer(svr *Server) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	_, ok := r.svrs[svr.Addr]

	if ok {
		return ErrServerExists
	}

	svr.prevStatus = Down
	svr.Status = Down
	svr.useCheckDuration = svr.CheckDuration
	r.svrs[svr.Addr] = svr

	binded := make(map[string]*Cluster)
	r.mapping[svr.Addr] = binded

	svr.init()

	if !svr.External {
		r.addToCheck(svr)
	} else {
		svr.changeTo(Up)
	}

	r.analysiser.addNewAnalysis(svr.Addr)
	// 1 secs default add to use
	r.analysiser.AddRecentCount(svr.Addr, 1)
	if svr.OpenToCloseCollectSeconds > 0 {
		r.analysiser.AddRecentCount(svr.Addr, svr.OpenToCloseCollectSeconds)
	}

	if svr.HalfToOpenCollectSeconds > 0 {
		r.analysiser.AddRecentCount(svr.Addr, svr.HalfToOpenCollectSeconds)
	}

	log.Infof("meta: server <%s> added", svr.Addr)

	return nil
}

// UpdateCluster update cluster
func (r *RouteTable) UpdateCluster(cluster *Cluster) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	old, ok := r.clusters[cluster.Name]

	if !ok {
		return ErrClusterNotFound
	}

	old.updateFrom(cluster)

	return nil
}

// DeleteCluster delete a cluster
func (r *RouteTable) DeleteCluster(clusterName string) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	cluster, ok := r.clusters[clusterName]

	if !ok {
		return ErrClusterNotFound
	}

	cluster.doInEveryBindServers(func(addr string) {
		if svr, ok := r.svrs[addr]; ok {
			r.doUnBind(svr, cluster, false)
		}
	})

	delete(r.clusters, cluster.Name)

	// TODO: API node loose cluster

	log.Infof("meta: cluster <%s> deleted", cluster.Name)

	return nil
}

// AddNewCluster add new cluster
func (r *RouteTable) AddNewCluster(cluster *Cluster) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	_, ok := r.clusters[cluster.Name]

	if ok {
		return ErrClusterExists
	}

	r.clusters[cluster.Name] = cluster

	log.Infof("meta: cluster <%s> added", cluster.Name)

	return nil
}

// Bind bind server and cluster
func (r *RouteTable) Bind(svrAddr string, clusterName string) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	svr, ok := r.svrs[svrAddr]
	if !ok {
		log.Errorf("meta: bind <%s,%s> failed: server not found",
			svrAddr,
			clusterName)
		return ErrServerNotFound
	}

	cluster, ok := r.clusters[clusterName]
	if !ok {
		log.Errorf("meta: bind <%s,%s> failed: cluster not found",
			svrAddr,
			clusterName)
		return ErrClusterNotFound
	}

	binded, _ := r.mapping[svr.Addr]
	bindCluster, ok := binded[cluster.Name]

	if ok && bindCluster.Name == clusterName {
		return ErrBindExists
	}

	binded[cluster.Name] = cluster

	log.Infof("meta: bind <%s,%s> stored",
		svrAddr,
		clusterName)

	if svr.Status == Up {
		cluster.bind(svr)
	}

	return nil
}

// UnBind unbind cluster and server
func (r *RouteTable) UnBind(svrAddr string, clusterName string) error {
	r.rwLock.Lock()
	defer r.rwLock.Unlock()

	svr, ok := r.svrs[svrAddr]
	if !ok {
		log.Errorf("meta: unbind <%s,%s> failed: server not found",
			svrAddr,
			clusterName)
		return ErrServerNotFound
	}

	cluster, ok := r.clusters[clusterName]
	if !ok {
		log.Errorf("meta: unbind <%s,%s> failed:cluster not found",
			svrAddr,
			clusterName)
		return ErrClusterNotFound
	}

	r.doUnBind(svr, cluster, true)

	return nil
}

func (r *RouteTable) doUnBind(svr *Server, cluster *Cluster, withLock bool) {
	if binded, ok := r.mapping[svr.Addr]; ok {
		delete(binded, cluster.Name)
		log.Infof("meta: bind <%s,%s> stored removed",
			svr.Addr,
			cluster.Name)
		if withLock {
			cluster.unbind(svr)
		} else {
			cluster.doUnBind(svr)
		}
	}
}

// Select return route result
func (r *RouteTable) Select(req *fasthttp.Request) []*RouteResult {
	r.rwLock.RLock()

	var results []*RouteResult

	for _, api := range r.apis {
		if api.matches(req) {
			results = make([]*RouteResult, len(api.Nodes))

			for index, node := range api.Nodes {
				results[index] = &RouteResult{
					API:  api,
					Node: node,
					Svr:  r.selectServer(req, r.selectClusterByRouting(req, r.clusters[node.ClusterName])),
				}
			}
		}
	}

	r.rwLock.RUnlock()
	return results
}

func (r *RouteTable) selectClusterByRouting(req *fasthttp.Request, src *Cluster) *Cluster {
	targetCluster := src

	for _, routing := range r.routings {
		if routing.Matches(req) {
			targetCluster = r.clusters[routing.ClusterName]
			break
		}
	}

	return targetCluster
}

func (r *RouteTable) selectServer(req *fasthttp.Request, cluster *Cluster) *Server {
	return r.doSelectServer(req, cluster)
}

func (r *RouteTable) doSelectServer(req *fasthttp.Request, cluster *Cluster) *Server {
	addr := cluster.Select(req) // 这里有可能会被锁住，会被正在修改bind关系的cluster锁住
	svr, _ := r.svrs[addr]
	return svr
}

// GetAnalysis return analysis
func (r *RouteTable) GetAnalysis() *Analysis {
	return r.analysiser
}

// GetTimeWheel return time wheel
func (r *RouteTable) GetTimeWheel() *goetty.HashedTimeWheel {
	return r.tw
}

func (r *RouteTable) watch() {
	log.Info("meta: routetable start watch")

	go r.doEvtReceive()
	err := r.store.Watch(r.watchReceiveCh, r.watchStopCh)
	log.Errorf("meta:  routetable watch failed, errors:\n%+v",
		err)
}

func (r *RouteTable) doEvtReceive() {
	for {
		evt := <-r.watchReceiveCh

		if evt.Src == EventSrcCluster {
			r.doReceiveCluster(evt)
		} else if evt.Src == EventSrcServer {
			r.doReceiveServer(evt)
		} else if evt.Src == EventSrcBind {
			r.doReceiveBind(evt)
		} else if evt.Src == EventSrcAPI {
			r.doReceiveAPI(evt)
		} else if evt.Src == EventSrcRouting {
			r.doReceiveRouting(evt)
		} else {
			log.Warnf("meta: evt unknown <%+v>", evt)
		}
	}
}

func (r *RouteTable) doReceiveRouting(evt *Evt) {
	routing, _ := evt.Value.(*Routing)

	if evt.Type == EventTypeNew {
		r.AddNewRouting(routing)
	} else if evt.Type == EventTypeDelete {
		r.DeleteRouting(evt.Key)
	} else if evt.Type == EventTypeUpdate {
		// TODO: impl
	}
}

func (r *RouteTable) doReceiveAPI(evt *Evt) {
	api, _ := evt.Value.(*API)

	if evt.Type == EventTypeNew {
		r.AddNewAPI(api)
	} else if evt.Type == EventTypeDelete {
		r.DeleteAPI(parseAPIKey(evt.Key))
	} else if evt.Type == EventTypeUpdate {
		r.UpdateAPI(api)
	}
}

func (r *RouteTable) doReceiveCluster(evt *Evt) {
	cluster, _ := evt.Value.(*Cluster)

	if evt.Type == EventTypeNew {
		r.AddNewCluster(cluster)
	} else if evt.Type == EventTypeDelete {
		r.DeleteCluster(evt.Key)
	} else if evt.Type == EventTypeUpdate {
		r.UpdateCluster(cluster)
	}
}

func (r *RouteTable) doReceiveServer(evt *Evt) {
	svr, _ := evt.Value.(*Server)

	if evt.Type == EventTypeNew {
		r.AddNewServer(svr)
	} else if evt.Type == EventTypeDelete {
		r.DeleteServer(evt.Key)
	} else if evt.Type == EventTypeUpdate {
		r.UpdateServer(svr)
	}
}

func (r *RouteTable) doReceiveBind(evt *Evt) {
	bind, _ := evt.Value.(*Bind)

	if evt.Type == EventTypeNew {
		r.Bind(bind.ServerAddr, bind.ClusterName)
	} else if evt.Type == EventTypeDelete {
		if bind == nil {
			bind = UnMarshalBindFromString(evt.Key)
		}
		r.UnBind(bind.ServerAddr, bind.ClusterName)
	}
}

// Load load info from store
func (r *RouteTable) Load() {
	r.loadClusters()
	r.loadServers()
	r.loadBinds()
	r.loadAPIs()
	r.loadRoutings()

	go r.watch()
}

func (r *RouteTable) loadClusters() {
	clusters, err := r.store.GetClusters()
	if nil != err {
		log.Errorf("meta: load clusters failed, errors:\n%+v",
			err)
		return
	}

	for _, cluster := range clusters {
		err := r.AddNewCluster(cluster)
		if nil != err {
			log.Fatalf("meta: cluster <%s> add failed, errors:\n%+v",
				cluster.Name,
				err)
		}
	}
}

func (r *RouteTable) loadServers() {
	servers, err := r.store.GetServers()
	if nil != err {
		log.Errorf("meta: load servers from store failed, errors:\n%+v",
			err)
		return
	}

	for _, server := range servers {
		err := r.AddNewServer(server)

		if nil != err {
			log.Fatalf("meta: server <%s> add failed, errors:\n%+v",
				server.Addr,
				err)
		}
	}
}

func (r *RouteTable) loadRoutings() {
	routings, err := r.store.GetRoutings()
	if nil != err {
		log.Errorf("meta: load routings from store failed, errors:\n%+v",
			err)
		return
	}

	for _, route := range routings {
		err := r.AddNewRouting(route)
		if nil != err {
			log.Fatalf("meta: routing <%s> add failed, errors:\n%+v",
				route.Cfg,
				err)
		}
	}
}

func (r *RouteTable) loadBinds() {
	binds, err := r.store.GetBinds()
	if nil != err {
		log.Errorf("meta: load binds from store failed, errors:\n%+v",
			err)
		return
	}

	for _, b := range binds {
		err := r.Bind(b.ServerAddr, b.ClusterName)
		if nil != err {
			log.Fatalf("meta: bind <%s, %s> add failed, errors:\n%+v",
				b.ServerAddr,
				b.ClusterName,
				err)
		}
	}
}

func (r *RouteTable) loadAPIs() {
	apis, err := r.store.GetAPIs()
	if nil != err {
		log.Errorf("meta: load apis from store failed, errors:\n%+v",
			err)
		return
	}

	for _, api := range apis {
		err := r.AddNewAPI(api)
		if nil != err {
			log.Fatalf("meta: api <%s> add failed, errors:\n%+v",
				api.URL,
				err)
		}
	}
}

func (r *RouteTable) removeFromCheck(svr *Server) {
	r.tw.Cancel(svr.Addr)
}

func (r *RouteTable) addToCheck(svr *Server) {
	if svr.check() {
		svr.changeTo(Up)

		if svr.statusChanged() {
			log.Infof("meta: server <%s> UP",
				svr.Addr)
		}
	} else {
		svr.changeTo(Down)

		if svr.statusChanged() {
			log.Warnf("meta: server <%s, %s> DOWN",
				svr.Addr,
				svr.CheckPath)
		}
	}

	r.tw.AddWithID(time.Duration(svr.useCheckDuration)*time.Second, svr.Addr, r.check)
}

func (r *RouteTable) check(addr string) {
	svr, _ := r.svrs[addr]

	if !svr.checkStopped {
		r.addToCheck(svr)
	}

	r.evtChan <- svr
}

func (r *RouteTable) changed() {
	for {
		svr := <-r.evtChan

		if svr.statusChanged() {
			binded := r.mapping[svr.Addr]

			if svr.Status == Up {
				for _, c := range binded {
					c.bind(svr)
				}
			} else {
				for _, c := range binded {
					c.unbind(svr)
				}
			}
		}
	}
}
