package proxy

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
)

const (
	IngressTrusted             = "trusted"
	IngressAdapter             = "adapter"
	ExecutionCredentialHeader  = "X-Workload-Execution-Credential"
	CompletionCredentialHeader = "X-Workload-Completion-Credential"
)

// ValidIngressCredential checks the wire format, not the randomness of a secret.
// Generate credentials independently from 32 bytes of cryptographic randomness.
func ValidIngressCredential(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validateIngress(config Config) error {
	switch config.IngressMode {
	case "", IngressTrusted:
		if config.ExecutionCredential != "" || config.CompletionCredential != "" {
			return errors.New("ingress credentials require adapter mode")
		}
	case IngressAdapter:
		return validateAdapterIngress(config)
	default:
		return errors.New("ingress-mode must be trusted or adapter")
	}
	return nil
}

func validateAdapterIngress(config Config) error {
	if !ValidIngressCredential(config.ExecutionCredential) || !ValidIngressCredential(config.CompletionCredential) {
		return errors.New("adapter ingress requires two 64-character lowercase hexadecimal credentials")
	}
	if subtle.ConstantTimeCompare([]byte(config.ExecutionCredential), []byte(config.CompletionCredential)) == 1 {
		return errors.New("execution and completion credentials must be distinct")
	}
	if config.Upstream != nil && config.Upstream.User != nil {
		return errors.New("adapter upstream URL must not contain credentials")
	}
	for _, header := range []string{config.RequestIDHeader, config.JobIDHeader, config.FenceIDHeader, config.FenceEpochHeader} {
		if sensitiveIngressHeader(header) {
			return errors.New("control headers must not alias ingress credentials")
		}
	}
	return nil
}

func (h *Handler) authorizeIngress(response http.ResponseWriter, request *http.Request) bool {
	if h.ingressMode != IngressAdapter {
		return true
	}
	header, other, credential := ExecutionCredentialHeader, CompletionCredentialHeader, h.executionCredential
	if request.Method == http.MethodPost && request.URL.Path == h.completionPath {
		header, other, credential = CompletionCredentialHeader, ExecutionCredentialHeader, h.completionCredential
	}
	values := ingressHeaderValues(request.Header, header)
	if len(values) != 1 || len(ingressHeaderValues(request.Header, other)) != 0 ||
		subtle.ConstantTimeCompare([]byte(values[0]), []byte(credential)) != 1 {
		writeError(response, http.StatusUnauthorized, "ingress_unauthorized")
		return false
	}
	return true
}

func ingressHeaderValues(header http.Header, name string) []string {
	var values []string
	for key, fields := range header {
		if strings.EqualFold(key, name) {
			values = append(values, fields...)
		}
	}
	return values
}

func sensitiveIngressHeader(name string) bool {
	return strings.EqualFold(name, ExecutionCredentialHeader) || strings.EqualFold(name, CompletionCredentialHeader) ||
		strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization")
}

func stripIngressHeaders(header http.Header, registration bool) {
	for name := range header {
		if sensitiveIngressHeader(name) || registration && strings.EqualFold(name, DefaultRegistrationTokenHeader) {
			delete(header, name)
		}
	}
}

func stripIngressResponseMetadata(header http.Header, secrets []string) {
	stripIngressHeaders(header, true)
	for name, values := range header {
		values = slices.DeleteFunc(values, func(value string) bool { return containsIngressSecret(value, secrets, false) })
		if len(values) == 0 || containsIngressSecret(name, secrets, true) {
			delete(header, name)
		} else {
			header[name] = values
		}
	}
}

func containsIngressSecret(value string, secrets []string, fieldName bool) bool {
	if fieldName {
		value = strings.ToLower(value)
	}
	for _, secret := range secrets {
		if fieldName {
			secret = strings.ToLower(secret)
		}
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func (h *Handler) protectIngressResponse(response *http.Response, token string) {
	if h.ingressMode != IngressAdapter {
		return
	}
	secrets := []string{token, h.executionCredential, h.completionCredential}
	stripIngressResponseMetadata(response.Header, secrets)
	stripIngressResponseMetadata(response.Trailer, secrets)
	// Upgrade bodies must keep their bidirectional interface. HTTP trailers do
	// not exist after a protocol switch; ordinary streaming trailers arrive on Read.
	if response.Body != nil && response.StatusCode != http.StatusSwitchingProtocols {
		response.Body = &ingressResponseBody{ReadCloser: response.Body, response: response, secrets: secrets}
	}
}

type ingressResponseBody struct {
	io.ReadCloser
	response *http.Response
	secrets  []string
}

type ingressTrailerKey struct{}

type ingressTransport struct {
	base                                      http.RoundTripper
	executionCredential, completionCredential string
}

func (transport ingressTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	secrets := []string{request.Header.Get(DefaultRegistrationTokenHeader), transport.executionCredential, transport.completionCredential}
	trace := &httptrace.ClientTrace{Got1xxResponse: func(_ int, header textproto.MIMEHeader) error {
		// WithClientTrace runs this hook before the existing ReverseProxy hook.
		stripIngressResponseMetadata(http.Header(header), secrets)
		return nil
	}}
	forwarded := request.Clone(httptrace.WithClientTrace(request.Context(), trace))
	stripIngressHeaders(forwarded.Header, false)
	stripIngressHeaders(forwarded.Trailer, true)
	if source, ok := request.Context().Value(ingressTrailerKey{}).(http.Header); ok && forwarded.Body != nil {
		forwarded.Body = &ingressRequestBody{ReadCloser: forwarded.Body, source: source, target: forwarded.Trailer}
	}
	return transport.base.RoundTrip(forwarded)
}

func ingressTrailerContext(request *http.Request) context.Context {
	return context.WithValue(request.Context(), ingressTrailerKey{}, request.Trailer)
}

type ingressRequestBody struct {
	io.ReadCloser
	source, target http.Header
}

func (body *ingressRequestBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	// Incoming trailers are populated only when the original body reaches EOF.
	// Copy safe values into the final transport request after clone boundaries.
	for name, values := range body.source {
		if !sensitiveIngressHeader(name) && !strings.EqualFold(name, DefaultRegistrationTokenHeader) {
			body.target[name] = append([]string(nil), values...)
		}
	}
	stripIngressHeaders(body.target, true)
	return n, err
}

func (body *ingressResponseBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	stripIngressResponseMetadata(body.response.Trailer, body.secrets)
	return n, err
}
