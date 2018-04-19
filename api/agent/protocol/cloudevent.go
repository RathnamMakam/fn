package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opencensus.io/trace"

	"github.com/fnproject/fn/api/models"
)

// CloudEvent is the official JSON representation of a CloudEvent: https://github.com/cloudevents/spec/blob/master/serialization.md
type CloudEvent struct {
	CloudEventsVersion string                 `json:"cloudEventsVersion"`
	EventID            string                 `json:"eventID"`
	Source             string                 `json:"source"`
	EventType          string                 `json:"eventType"`
	EventTypeVersion   string                 `json:"eventTypeVersion"`
	EventTime          time.Time              `json:"eventTime"` // TODO: ensure rfc3339 format
	SchemaURL          string                 `json:"schemaURL"`
	ContentType        string                 `json:"contentType"`
	Extensions         map[string]interface{} `json:"extensions"`
	Data               interface{}            `json:"data"` // from docs: the payload is encoded into a media format which is specified by the contentType attribute (e.g. application/json)
}

type cloudEventIn struct {
	CloudEvent

	// Deadline string `json:"deadline"`
	// Protocol CallRequestHTTP `json:"protocol"`
}

// cloudEventOut the expected response from the function container
type cloudEventOut struct {
	CloudEvent

	// Protocol    *CallResponseHTTP `json:"protocol,omitempty"`
}

// CloudEventProtocol converts stdin/stdout streams from HTTP into JSON format.
type CloudEventProtocol struct {
	// These are the container input streams, not the input from the request or the output for the response
	in  io.Writer
	out io.Reader
}

func (p *CloudEventProtocol) IsStreamable() bool {
	return true
}

func (h *CloudEventProtocol) writeJSONToContainer(ci CallInfo) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	_, err := io.Copy(buf, ci.Input())
	if err != nil {
		return err
	}

	in := cloudEventIn{
		CloudEvent: CloudEvent{
			ContentType: ci.ContentType(),
			EventID:     ci.CallID(),
			Extensions: map[string]interface{}{
				// note: protocol stuff should be set on first ingestion of the event in fn2.0, not here
				"protocol": CallRequestHTTP{
					Type:       ci.ProtocolType(),
					Method:     ci.Method(),
					RequestURL: ci.RequestURL(),
					Headers:    ci.Headers(),
				},
				"deadline": ci.Deadline().String(),
			},
		},
	}
	if buf.Len() == 0 {
		// nada
		// todo: should we leave as null, pass in empty string, omitempty or some default for the content type, eg: {} for json?
	} else if ci.ContentType() == "application/json" {
		d := map[string]interface{}{}
		err = json.NewDecoder(buf).Decode(&d)
		if err != nil {
			return fmt.Errorf("Invalid json body with contentType 'application/json'. %v", err)
		}
		in.Data = d
	} else {
		in.Data = buf.String()
	}
	return json.NewEncoder(h.in).Encode(in)
}

func (h *CloudEventProtocol) Dispatch(ctx context.Context, ci CallInfo, w io.Writer) error {
	ctx, span := trace.StartSpan(ctx, "dispatch_json")
	defer span.End()

	_, span = trace.StartSpan(ctx, "dispatch_json_write_request")
	err := h.writeJSONToContainer(ci)
	span.End()
	if err != nil {
		return err
	}

	_, span = trace.StartSpan(ctx, "dispatch_json_read_response")
	var jout cloudEventOut
	decoder := json.NewDecoder(h.out)
	err = decoder.Decode(&jout)
	span.End()
	if err != nil {
		return models.NewAPIError(http.StatusBadGateway, fmt.Errorf("invalid json response from function err: %v", err))
	}

	_, span = trace.StartSpan(ctx, "dispatch_json_write_response")
	defer span.End()

	rw, ok := w.(http.ResponseWriter)
	if !ok {
		// logs can just copy the full thing in there, headers and all.
		err := json.NewEncoder(w).Encode(jout)
		return isExcessData(err, decoder)
	}

	// this has to be done for pulling out:
	// - status code
	// - body
	// - headers
	pp := jout.Extensions["protocol"]
	var p map[string]interface{}
	if pp != nil {
		p = pp.(map[string]interface{})
		hh := p["headers"]
		if hh != nil {
			h, ok := hh.(map[string]interface{})
			if !ok {
				return fmt.Errorf("Invalid JSON for protocol headers, not a map")
			}
			for k, v := range h {
				// fmt.Printf("HEADER: %v: %v\n", k, v)
				// fmt.Printf("%v", reflect.TypeOf(v))
				harray, ok := v.([]interface{})
				if !ok {
					return fmt.Errorf("Invalid JSON for protocol headers, not an array of strings for header value")
				}
				for _, vv := range harray {
					rw.Header().Add(k, vv.(string)) // on top of any specified on the route
				}
			}
		}
	}
	// after other header setting, top level content_type takes precedence and is
	// absolute (if set). it is expected that if users want to set multiple
	// values they put it in the string, e.g. `"content-type:"application/json; charset=utf-8"`
	// TODO this value should not exist since it's redundant in proto headers?
	if jout.ContentType != "" {
		rw.Header().Set("Content-Type", jout.ContentType)
	}

	// we must set all headers before writing the status, see http.ResponseWriter contract
	if p != nil && p["status_code"] != nil {
		sc := p["status_code"].(float64)
		rw.WriteHeader(int(sc))
	}

	if jout.ContentType == "application/json" {
		d, err := json.Marshal(jout.Data)
		if err != nil {
			return fmt.Errorf("Error marshalling function response 'data' to json. %v\n", err)
		}
		_, err = rw.Write(d)
	} else if jout.ContentType == "text/plain" {
		_, err = io.WriteString(rw, jout.Data.(string))
	} else {
		return fmt.Errorf("Error: Unknown content type: %v\n", jout.ContentType)
	}
	return isExcessData(err, decoder)
}
