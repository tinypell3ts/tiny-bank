package main

import (
	"github.com/monzo/typhon"
)

// routes is hello's API. kit.Handler adds the shared filters, the probes and
// /metrics around it.
func routes(router *typhon.Router) {
	router.GET("/", handleHello)
}

func handleHello(req typhon.Request) typhon.Response {
	return req.Response(map[string]string{"message": "hello from tiny-bank"})
}
