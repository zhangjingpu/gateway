package proxy

import (
	"errors"
	"plugin"
	"strings"

	"github.com/fagongzi/gateway/pkg/conf"
	"github.com/fagongzi/gateway/pkg/filter"
)

var (
	// ErrUnknownFilter unknown filter error
	ErrUnknownFilter = errors.New("unknow filter")
)

const (
	// FilterHTTPAccess access log filter
	FilterHTTPAccess = "HTTP-ACCESS"
	// FilterHeader header filter
	FilterHeader = "HEAD" // process header fiter
	// FilterXForward xforward fiter
	FilterXForward = "XFORWARD"
	// FilterBlackList blacklist filter
	FilterBlackList = "BLACKLIST"
	// FilterWhiteList whitelist filter
	FilterWhiteList = "WHITELIST"
	// FilterAnalysis analysis filter
	FilterAnalysis = "ANALYSIS"
	// FilterRateLimiting limit filter
	FilterRateLimiting = "RATE-LIMITING"
	// FilterCircuitBreake circuit breake filter
	FilterCircuitBreake = "CIRCUIT-BREAKE"
	// FilterValidation validation request filter
	FilterValidation = "VALIDATION"
)

func newFilter(filterSpec *conf.FilterSpec) (filter.Filter, error) {
	if filterSpec.External {
		return newExternalFilter(filterSpec)
	}

	input := strings.ToUpper(filterSpec.Name)

	switch input {
	case FilterHTTPAccess:
		return newAccessFilter(), nil
	case FilterHeader:
		return newHeadersFilter(), nil
	case FilterXForward:
		return newXForwardForFilter(), nil
	case FilterAnalysis:
		return newAnalysisFilter(), nil
	case FilterBlackList:
		return newBlackListFilter(), nil
	case FilterWhiteList:
		return newWhiteListFilter(), nil
	case FilterRateLimiting:
		return newRateLimitingFilter(), nil
	case FilterCircuitBreake:
		return newCircuitBreakeFilter(), nil
	case FilterValidation:
		return newValidationFilter(), nil
	default:
		return nil, ErrUnknownFilter
	}
}

func newExternalFilter(filterSpec *conf.FilterSpec) (filter.Filter, error) {
	p, err := plugin.Open(filterSpec.ExternalPluginFile)
	if err != nil {
		return nil, err
	}

	s, err := p.Lookup("NewExternalFilter")
	if err != nil {
		return nil, err
	}

	sf := s.(func() (filter.Filter, error))
	return sf()
}
