package httptransport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkDirectIdleReuse exercises repeated bursts to one trusted upstream.
// The dial count captures connection churn that latency alone can hide.
func BenchmarkDirectIdleReuse(b *testing.B) {
	for _, idleLimit := range []int{2, 8, 16, 32, 64} {
		b.Run(fmt.Sprintf("idle_%d", idleLimit), func(b *testing.B) {
			b.StopTimer()
			b.ReportAllocs()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(time.Millisecond)
				w.WriteHeader(http.StatusNoContent)
			}))
			transport := NewDirect()
			transport.MaxIdleConnsPerHost = idleLimit
			transport.MaxIdleConns = 64
			dial := transport.DialContext
			var dials atomic.Int64
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err == nil {
					dials.Add(1)
				}
				return conn, err
			}
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			for i := -1; i < b.N; i++ {
				if i == 0 {
					dials.Store(0)
					b.StartTimer()
				}
				var wg sync.WaitGroup
				errs := make(chan error, 32)
				for j := 0; j < 32; j++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						response, err := client.Get(server.URL)
						if err != nil {
							errs <- err
							return
						}
						_, err = io.Copy(io.Discard, response.Body)
						closeErr := response.Body.Close()
						if err != nil {
							errs <- err
						} else if closeErr != nil {
							errs <- closeErr
						}
					}()
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(dials.Load())/float64(b.N), "dials/op")
			transport.CloseIdleConnections()
			server.Close()
		})
	}
}
