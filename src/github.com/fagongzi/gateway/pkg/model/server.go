package model

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/fagongzi/log"
)

// Status status
type Status int

// Circuit circuit status
type Circuit int

const (
	// Down backend server down status
	Down = Status(0)
	// Up backend server up status
	Up = Status(1)
)

const (
	// CircuitOpen Circuit open status
	CircuitOpen = Circuit(0)
	// CircuitHalf Circuit half status
	CircuitHalf = Circuit(1)
	// CircuitClose Circuit close status
	CircuitClose = Circuit(2)
)

const (
	// DefaultCheckDurationInSeconds Default duration to check server
	DefaultCheckDurationInSeconds = 5
	// DefaultCheckTimeoutInSeconds Default timeout to check server
	DefaultCheckTimeoutInSeconds = 3
)

// Server server
type Server struct {
	Schema string `json:"schema,omitempty"`
	Addr   string `json:"addr,omitempty"`

	// External external, e.g. create from external service discovery
	External bool `json:"external,omitempty"`
	// CheckPath begin with / checkpath, expect return CheckResponsedBody.
	CheckPath string `json:"checkPath,omitempty"`
	// CheckResponsedBody check url responsed http body, if not set, not check body
	CheckResponsedBody string `json:"checkResponsedBody"`
	// CheckDuration check interval, unit second
	CheckDuration int `json:"checkDuration,omitempty"`
	// CheckTimeout timeout to check server
	CheckTimeout int `json:"checkTimeout,omitempty"`
	// Status Server status
	Status Status `json:"status,omitempty"`

	// MaxQPS the backend server max qps support
	MaxQPS                    int `json:"maxQPS,omitempty"`
	HalfToOpenSeconds         int `json:"halfToOpenSeconds,omitempty"`
	HalfTrafficRate           int `json:"halfTrafficRate,omitempty"`
	HalfToOpenSucceedRate     int `json:"halfToOpenSucceedRate,omitempty"`
	HalfToOpenCollectSeconds  int `json:"halfToOpenCollectSeconds,omitempty"`
	OpenToCloseFailureRate    int `json:"openToCloseFailureRate,omitempty"`
	OpenToCloseCollectSeconds int `json:"openToCloseCollectSeconds,omitempty"`

	BindClusters []string `json:"bindClusters,omitempty"`

	httpClient       *http.Client
	checkFailCount   int
	prevStatus       Status
	useCheckDuration int

	circuit Circuit
	lock    *sync.Mutex

	checkStopped bool
}

// UnMarshalServer unmarshal
func UnMarshalServer(data []byte) *Server {
	v := &Server{}
	json.Unmarshal(data, v)

	if 0 == v.CheckTimeout {
		v.CheckTimeout = DefaultCheckTimeoutInSeconds
	}

	return v
}

// UnMarshalServerFromReader unmarshal
func UnMarshalServerFromReader(r io.Reader) (*Server, error) {
	v := &Server{}

	decoder := json.NewDecoder(r)
	err := decoder.Decode(v)

	v.Status = Down

	if 0 == v.CheckTimeout {
		v.CheckTimeout = DefaultCheckTimeoutInSeconds
	}

	return v, err
}

// Marshal marshal
func (s *Server) Marshal() []byte {
	v, _ := json.Marshal(s)
	return v
}

// HasBind add bind
func (s *Server) HasBind() bool {
	return len(s.BindClusters) > 0
}

// AddBind add bind
func (s *Server) AddBind(bind *Bind) {
	index := s.indexOf(bind.ClusterName)
	if index == -1 {
		s.BindClusters = append(s.BindClusters, bind.ClusterName)
	}
}

// RemoveBind remove bind
func (s *Server) RemoveBind(clusterName string) {
	index := s.indexOf(clusterName)
	if index >= 0 {
		s.BindClusters = append(s.BindClusters[:index], s.BindClusters[index+1:]...)
	}
}

func (s *Server) indexOf(clusterName string) int {
	for index, s := range s.BindClusters {
		if s == clusterName {
			return index
		}
	}

	return -1
}

func (s *Server) updateFrom(svr *Server) {
	if s.lock != nil {
		s.Lock()
		defer s.UnLock()
	}

	s.MaxQPS = svr.MaxQPS
	s.HalfToOpenSeconds = svr.HalfToOpenSeconds
	s.HalfTrafficRate = svr.HalfTrafficRate
	s.HalfToOpenCollectSeconds = svr.HalfToOpenCollectSeconds
	s.HalfToOpenSucceedRate = svr.HalfToOpenSucceedRate
	s.OpenToCloseCollectSeconds = svr.OpenToCloseCollectSeconds
	s.OpenToCloseFailureRate = svr.OpenToCloseFailureRate

	log.Infof("meta: server <%s> updated",
		s.Addr)
}

// GetCircuit return circuit status
func (s *Server) GetCircuit() Circuit {
	return s.circuit
}

// OpenCircuit set circuit open status
func (s *Server) OpenCircuit() {
	s.circuit = CircuitOpen
}

// CloseCircuit set circuit close status
func (s *Server) CloseCircuit() {
	s.circuit = CircuitClose
}

// HalfCircuit set circuit half status
func (s *Server) HalfCircuit() {
	s.circuit = CircuitHalf
}

// Lock lock
func (s *Server) Lock() {
	s.lock.Lock()
}

// UnLock unlock
func (s *Server) UnLock() {
	s.lock.Unlock()
}

func (s *Server) init() {
	s.httpClient = &http.Client{
		Timeout: time.Second * s.getCheckTimeout(),
	}

	s.circuit = CircuitOpen
	s.lock = &sync.Mutex{}
	s.checkStopped = false
}

func (s *Server) stopCheck() {
	s.checkStopped = true
}

func (s *Server) getCheckTimeout() time.Duration {
	if s.CheckTimeout == 0 {
		return time.Duration(DefaultCheckTimeoutInSeconds)
	}

	return time.Duration(s.CheckTimeout)
}

func (s *Server) check() bool {
	succ := false
	defer func() {
		if succ {
			s.reset()
		} else {
			s.fail()
		}
	}()

	log.Debugf("meta: server <%s, %s> start check",
		s.Addr,
		s.CheckPath)

	resp, err := s.httpClient.Get(s.getCheckURL())

	if err != nil {
		log.Warnf("meta: server <%s, %s, %d> check failed, errors:\n%+v",
			s.Addr,
			s.CheckPath,
			s.checkFailCount+1,
			err)
		return succ
	}

	defer resp.Body.Close()

	if http.StatusOK != resp.StatusCode {
		log.Warnf("meta: server <%s, %s, %d, %d> check failed",
			s.Addr,
			s.CheckPath,
			resp.StatusCode,
			s.checkFailCount+1)
		return succ
	}

	if s.CheckResponsedBody == "" {
		succ = true
		return true
	}

	body, err := ioutil.ReadAll(resp.Body)
	if nil != err {
		return false
	}

	succ = string(body) == s.CheckResponsedBody
	return succ
}

func (s *Server) getCheckURL() string {
	return fmt.Sprintf("%s://%s%s", s.Schema, s.Addr, s.CheckPath)
}

func (s *Server) fail() {
	s.checkFailCount++
	s.useCheckDuration += s.useCheckDuration / 2
}

func (s *Server) reset() {
	s.checkFailCount = 0
	s.useCheckDuration = s.CheckDuration
}

func (s *Server) changeTo(status Status) {
	s.prevStatus = s.Status
	s.Status = status
}

func (s *Server) statusChanged() bool {
	return s.prevStatus != s.Status
}
