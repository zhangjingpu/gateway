package proxy

import (
	"container/list"
	"errors"
	"net"
	"net/http"
	"net/rpc"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fagongzi/gateway/pkg/conf"
	"github.com/fagongzi/gateway/pkg/model"
	"github.com/fagongzi/gateway/pkg/util"
	"github.com/fagongzi/log"
	"github.com/fagongzi/util/task"
	"github.com/valyala/fasthttp"
)

var (
	// ErrPrefixRequestCancel user cancel request error
	ErrPrefixRequestCancel = "request canceled"
	// ErrNoServer no server
	ErrNoServer = errors.New("has no server")
	// ErrRewriteNotMatch rewrite not match request url
	ErrRewriteNotMatch = errors.New("rewrite not match request url")
)

var (
	// MergeContentType merge operation using content-type
	MergeContentType = "application/json; charset=utf-8"
	// MergeRemoveHeaders merge operation need to remove headers
	MergeRemoveHeaders = []string{
		"Content-Length",
		"Content-Type",
		"Date",
	}
)

// NewProxy create a new proxy
func NewProxy(cnf *conf.Conf) *Proxy {
	p := &Proxy{
		fastHTTPClients: make(map[string]*util.FastHTTPClient),
		cnf:             cnf,
		filters:         list.New(),
		stopC:           make(chan struct{}),
		taskRunner:      task.NewRunner(),
	}

	p.init()

	return p
}

// Proxy Proxy
type Proxy struct {
	sync.RWMutex

	cnf             *conf.Conf
	filters         *list.List
	fastHTTPClients map[string]*util.FastHTTPClient
	routeTable      *model.RouteTable

	rpcListener net.Listener

	taskRunner *task.Runner
	stopped    int32
	stopC      chan struct{}
	stopOnce   sync.Once
	stopWG     sync.WaitGroup
}

// Start start proxy
func (p *Proxy) Start() {
	go p.listenToStop()

	err := p.startRPC()
	if nil != err {
		log.Fatalf("bootstrap: rpc start failed, addr=<%s> errors:\n%+v",
			p.cnf.MgrAddr,
			err)
	}

	log.Infof("bootstrap: gateway proxy started at <%s>", p.cnf.Addr)
	err = fasthttp.ListenAndServe(p.cnf.Addr, p.ReverseProxyHandler)
	if err != nil {
		log.Errorf("bootstrap: gateway proxy start failed, errors:\n%+v",
			err)
		return
	}
}

// Stop stop the proxy
func (p *Proxy) Stop() {
	log.Infof("stop: start to stop gateway proxy")

	p.stopWG.Add(1)
	p.stopC <- struct{}{}
	p.stopWG.Wait()

	log.Infof("stop: gateway proxy stopped")
}

func (p *Proxy) startRPC() error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", p.cnf.MgrAddr)

	if err != nil {
		return err
	}

	p.rpcListener, err = net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return err
	}

	log.Infof("rpc: listen at %s",
		p.cnf.MgrAddr)
	server := rpc.NewServer()
	mgrService := newManager(p)
	server.Register(mgrService)

	go func() {
		for {
			if p.isStopped() {
				return
			}

			conn, err := p.rpcListener.Accept()
			if err != nil {
				log.Errorf("rpc: accept new conn failed, errors:\n%+v",
					err)
				continue
			}

			if p.isStopped() {
				conn.Close()
				return
			}

			go server.ServeConn(conn)
		}
	}()

	return nil
}

func (p *Proxy) listenToStop() {
	<-p.stopC
	p.doStop()
}

func (p *Proxy) doStop() {
	p.stopOnce.Do(func() {
		defer p.stopWG.Done()
		p.setStopped()
		p.stopRPC()
		p.taskRunner.Stop()
	})
}

func (p *Proxy) stopRPC() error {
	return p.rpcListener.Close()
}

func (p *Proxy) setStopped() {
	atomic.StoreInt32(&p.stopped, 1)
}

func (p *Proxy) isStopped() bool {
	return atomic.LoadInt32(&p.stopped) == 1
}

func (p *Proxy) init() {
	err := p.initRouteTable()
	if err != nil {
		log.Fatalf("bootstrap: init route table failed, errors:\n%+v",
			err)
	}

	p.initFilters()
}

func (p *Proxy) initRouteTable() error {
	store, err := model.GetStoreFrom(p.cnf.RegistryAddr, p.cnf.Prefix, p.taskRunner)

	if err != nil {
		return err
	}

	register, _ := store.(model.Register)

	register.Registry(&model.ProxyInfo{
		Conf: p.cnf,
	})

	p.routeTable = model.NewRouteTable(p.cnf, store, p.taskRunner)
	p.routeTable.Load()

	return nil
}

func (p *Proxy) initFilters() {
	for _, filter := range p.cnf.Filers {
		f, err := newFilter(filter)
		if nil != err {
			log.Fatalf("bootstrap: init filter failed, filter=<%+v> errors:\n%+v",
				filter,
				err)
		}

		log.Infof("bootstrap: filter added, filter=<%+v>", filter)
		p.filters.PushBack(f)
	}
}

// ReverseProxyHandler http reverse handler
func (p *Proxy) ReverseProxyHandler(ctx *fasthttp.RequestCtx) {
	if p.isStopped() {
		ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
		return
	}

	results := p.routeTable.Select(&ctx.Request)

	if nil == results || len(results) == 0 {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		return
	}

	count := len(results)
	merge := count > 1

	if merge {
		wg := &sync.WaitGroup{}
		wg.Add(count)

		for _, result := range results {
			result.Merge = merge

			go func(result *model.RouteResult) {
				p.doProxy(ctx, wg, result)
			}(result)
		}

		wg.Wait()
	} else {
		p.doProxy(ctx, nil, results[0])
	}

	for _, result := range results {
		if result.Err != nil {
			if result.API.Mock != nil {
				result.API.RenderMock(ctx)
				result.Release()
				return
			}

			ctx.SetStatusCode(result.Code)
			result.Release()
			return
		}

		if !merge {
			p.writeResult(ctx, result.Res)
			result.Release()
			return
		}
	}

	for _, result := range results {
		for _, h := range MergeRemoveHeaders {
			result.Res.Header.Del(h)
		}
		result.Res.Header.CopyTo(&ctx.Response.Header)
	}

	ctx.Response.Header.SetContentType(MergeContentType)
	ctx.SetStatusCode(fasthttp.StatusOK)

	ctx.WriteString("{")

	for index, result := range results {
		ctx.WriteString("\"")
		ctx.WriteString(result.Node.AttrName)
		ctx.WriteString("\":")
		ctx.Write(result.Res.Body())
		if index < count-1 {
			ctx.WriteString(",")
		}

		result.Release()
	}

	ctx.WriteString("}")
}

func (p *Proxy) doProxy(ctx *fasthttp.RequestCtx, wg *sync.WaitGroup, result *model.RouteResult) {
	if nil != wg {
		defer wg.Done()
	}

	svr := result.Svr

	if nil == svr {
		result.Err = ErrNoServer
		result.Code = http.StatusServiceUnavailable
		return
	}

	outreq := copyRequest(&ctx.Request)

	// change url
	if result.NeedRewrite() {
		// if not use rewrite, it only change uri path and query string
		realPath := result.GetRewritePath(&ctx.Request)
		if "" != realPath {
			if log.DebugEnabled() {
				log.Debugf("proxy: rewrite, from=<%s> to=<%s>",
					string(ctx.URI().FullURI()),
					realPath)
			}

			outreq.SetRequestURI(realPath)
			outreq.SetHost(svr.Addr)
		} else {
			log.Warnf("proxy: rewrite not matches, origin=<%s> pattern=<%s>",
				string(ctx.URI().FullURI()),
				result.Node.Rewrite)

			result.Err = ErrRewriteNotMatch
			result.Code = http.StatusBadRequest
			return
		}
	}

	c := newContext(p.routeTable, ctx, outreq, result)

	// pre filters
	filterName, code, err := p.doPreFilters(c)
	if nil != err {
		log.Warnf("proxy: call pre filter failed, filter=<%s> errors:\n%+v",
			filterName,
			err)

		result.Err = err
		result.Code = code
		return
	}

	c.SetStartAt(time.Now().UnixNano())
	res, err := p.getClient(svr.Addr).Do(outreq, svr.Addr)
	c.SetEndAt(time.Now().UnixNano())

	result.Res = res

	if err != nil || res.StatusCode() >= fasthttp.StatusInternalServerError {
		resCode := http.StatusServiceUnavailable

		if nil != err {
			log.Warnf("proxy: failed, target=<%s> errors:\n%+v",
				svr.Addr,
				err)
		} else {
			resCode = res.StatusCode()
			log.Warnf("proxy: returns error code, target=<%s> code=<%d>",
				svr.Addr,
				res.StatusCode())
		}

		// 用户取消，不计算为错误
		if nil == err || !strings.HasPrefix(err.Error(), ErrPrefixRequestCancel) {
			p.doPostErrFilters(c)
		}

		result.Err = err
		result.Code = resCode
		return
	}

	if log.DebugEnabled() {
		log.Debugf("proxy: return, target=<%s> code=<%d> body=<%d>",
			svr.Addr,
			res.StatusCode(),
			res.Body())
	}

	// post filters
	filterName, code, err = p.doPostFilters(c)
	if nil != err {
		log.Warnf("proxy: call post filter failed, filter=<%s> errors:\n%+v",
			filterName,
			err)

		result.Err = err
		result.Code = code
		return
	}
}

func (p *Proxy) writeResult(ctx *fasthttp.RequestCtx, res *fasthttp.Response) {
	ctx.SetStatusCode(res.StatusCode())
	ctx.Write(res.Body())
}

func (p *Proxy) getClient(addr string) *util.FastHTTPClient {
	p.RLock()
	c, ok := p.fastHTTPClients[addr]
	if ok {
		p.RUnlock()
		return c
	}
	p.RUnlock()

	p.Lock()
	c, ok = p.fastHTTPClients[addr]
	if ok {
		p.Unlock()
		return c
	}

	p.fastHTTPClients[addr] = util.NewFastHTTPClient(p.cnf)
	c = p.fastHTTPClients[addr]
	p.Unlock()
	return c
}
