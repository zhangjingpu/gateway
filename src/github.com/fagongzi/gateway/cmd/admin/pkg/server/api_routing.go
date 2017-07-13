package server

import (
	"net/http"

	"github.com/fagongzi/gateway/pkg/model"
	"github.com/labstack/echo"
)

func (server *AdminServer) getRoutings() echo.HandlerFunc {
	return func(c echo.Context) error {
		var errstr string
		code := CodeSuccess

		routings, err := server.store.GetRoutings()
		if err != nil {
			errstr = err.Error()
			code = CodeError
		}

		return c.JSON(http.StatusOK, &Result{
			Code:  code,
			Error: errstr,
			Value: routings,
		})
	}
}

func (server *AdminServer) newRouting() echo.HandlerFunc {
	return func(c echo.Context) error {
		var errstr string
		code := CodeSuccess

		routing, err := model.UnMarshalRoutingFromReader(c.Request().Body())

		if err == nil {
			err = routing.Check()
		}

		if nil != err {
			errstr = err.Error()
			code = CodeError
		} else {
			err := server.store.SaveRouting(routing)
			if nil != err {
				errstr = err.Error()
				code = CodeError
			}
		}

		return c.JSON(http.StatusOK, &Result{
			Code:  code,
			Error: errstr,
		})
	}
}
