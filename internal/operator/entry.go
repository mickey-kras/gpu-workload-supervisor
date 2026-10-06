package operator

import (
	"errors"
	"io"
	"time"
)

// Serve implements one request, one response, no replay. Dependency arguments
// are native process/account operations; they are never supplied by the request.
// in and out must implement io.Closer: on I/O timeout the blocked read/write
// goroutine only unblocks when Close is called.
func Serve(args []string, in io.Reader, out io.Writer, load func() (Profile, error), detach func() error) {
	r := Response{ProtocolVersion: 1, Code: InvalidRequest}
	defer func() { _ = WriteResponse(out, r, 2*time.Second) }()
	if len(args) != 0 {
		return
	}
	body, err := ReadRequest(in, 2*time.Second)
	if err != nil {
		if errors.Is(err, ErrIOTimeout) {
			r.Code = Timeout
		}
		return
	}
	req, code := Decode(body)
	r.Code = code
	if code != OK {
		return
	}
	r.RequestID = req.RequestID
	if detach() != nil {
		r.Code = Unavailable
		return
	}
	profile, err := load()
	if err != nil {
		r.Code = IncompatibleConfiguration
		return
	}
	r = NativeService(profile).Handle(req)
}
