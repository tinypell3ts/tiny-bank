package kit

import (
	"fmt"
	"net/http"

	"github.com/monzo/typhon"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// jsonOptions writes the proto field names, such as customer_id, instead of
// Typhon's default lowerCamelCase (customerId).
var jsonOptions = protojson.MarshalOptions{UseProtoNames: true}

// Respond answers req with m, a protobuf message, as JSON with status code.
func Respond(req typhon.Request, m proto.Message, code int) typhon.Response {
	b, err := jsonOptions.Marshal(m)
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
	rsp.Header.Set("Content-Type", "application/json")
	return rsp
}

// RespondCreated is Respond with 201 Created and a Location header. Setting
// the header here matters: when encoding fails, Respond's response has no
// headers to set.
func RespondCreated(req typhon.Request, m proto.Message, location string) typhon.Response {
	rsp := Respond(req, m, http.StatusCreated)
	if rsp.Response != nil {
		rsp.Header.Set("Location", location)
	}
	return rsp
}
