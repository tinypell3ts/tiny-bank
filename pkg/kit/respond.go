package kit

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/monzo/typhon"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// protobufType is the media type for binary protobuf, as Typhon names it.
const protobufType = "application/protobuf"

// jsonOptions writes the proto field names, such as customer_id, instead of
// Typhon's default lowerCamelCase (customerId).
var jsonOptions = protojson.MarshalOptions{UseProtoNames: true}

// Respond answers req with m, a protobuf message, and status code. It's JSON
// with the proto field names, unless the caller asked for binary protobuf
// (Accept: application/protobuf), as other services do through kit.Get.
// Binary is smaller and faster to encode, and a caller built against an
// older contract still reads it, skipping fields it doesn't know.
func Respond(req typhon.Request, m proto.Message, code int) typhon.Response {
	contentType, marshal := "application/json", jsonOptions.Marshal
	if wantsProtobuf(req) {
		contentType, marshal = protobufType, proto.Marshal
	}
	b, err := marshal(m)
	if err != nil {
		// A fresh response, so ErrorFilter writes the error with its own
		// status. It only does that for a response still at 200, so an error
		// on a 201 would otherwise reach the caller as an empty 201.
		return typhon.Response{Error: fmt.Errorf("encode response: %w", err)}
	}
	rsp := typhon.NewResponseWithCode(req, code)
	if _, err := rsp.Write(b); err != nil {
		return typhon.Response{Error: fmt.Errorf("write response: %w", err)}
	}
	rsp.Header.Set("Content-Type", contentType)
	// The same URL answers in two formats, so a cache must key on Accept too.
	rsp.Header.Set("Vary", "Accept")
	return rsp
}

// wantsProtobuf reports whether the caller asked for binary protobuf. It uses
// Typhon's own test, so a request's errors (which Typhon's ErrorFilter
// encodes) and its successes always come back in the same format.
func wantsProtobuf(req typhon.Request) bool {
	return strings.Contains(req.Header.Get("Accept"), protobufType)
}

// RespondCreated is Respond with 201 Created and a Location header.
func RespondCreated(req typhon.Request, m proto.Message, location string) typhon.Response {
	return RespondAt(req, m, http.StatusCreated, location)
}

// RespondAt is Respond with a Location header, saying where the resource can
// be fetched: with 201 Created for one just made, or 202 Accepted for one
// still being worked on. Setting the header here matters: when encoding
// fails, Respond's response has no headers to set.
func RespondAt(req typhon.Request, m proto.Message, code int, location string) typhon.Response {
	rsp := Respond(req, m, code)
	if rsp.Response != nil {
		rsp.Header.Set("Location", location)
	}
	return rsp
}
