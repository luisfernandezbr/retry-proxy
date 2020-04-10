package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinpt/go-common/log"
	pos "github.com/pinpt/go-common/os"
	"github.com/spf13/cobra"
)

const (
	maxRetryAttempts   = 3
	maxRequestDuration = time.Minute
)

func newClient() *http.Client {
	client := &http.Client{}
	client.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false, // since we reuse on each call
		DisableCompression:    true,  // disable so we can stream byte-for-byte
	}
	return client
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use: "retry-proxy",
	Run: func(cmd *cobra.Command, args []string) {
		l := log.NewCommandLogger(cmd)
		defer l.Close()
		logger := log.With(l, "pkg", "proxy")

		// validate our upstream
		upstream, _ := cmd.Flags().GetString("upstream")
		if _, err := url.Parse(upstream); err != nil {
			log.Fatal(logger, "error parsing upstream url", "err", err, "url", upstream)
		}

		// useful for ping-pong between server to test
		echo, _ := cmd.Flags().GetBool("echo")

		// keep a pool of http clients ready
		pooledConnections := make([]*http.Client, 0)
		for i := 0; i < maxRetryAttempts; i++ {
			pooledConnections = append(pooledConnections, newClient())
		}

		var requests int32
		var mu sync.Mutex

		redirect := http.HandlerFunc(func(ow http.ResponseWriter, req *http.Request) {
			started := time.Now()
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				ow.WriteHeader(http.StatusBadRequest)
				return
			}
			if echo {
				ow.WriteHeader(http.StatusOK)
				io.Copy(ow, bytes.NewReader(body))
				return
			}
			u, _ := url.Parse(upstream)
			u.Path = req.URL.Path
			var tx int64
			rx := len(body)
			ctx, cancel := context.WithTimeout(req.Context(), maxRequestDuration)
			defer cancel()
			for i := 1; i <= maxRetryAttempts; i++ {
				// round robin our pools so we always get a different backend
				mu.Lock()
				requests++
				offset := requests % maxRetryAttempts
				cl := pooledConnections[offset]
				mu.Unlock()
				newreq, _ := http.NewRequestWithContext(ctx, req.Method, u.String(), bytes.NewReader(body))
				for k, v := range req.Header {
					newreq.Header.Set(k, v[0])
				}
				newreq.Header.Set("Content-Length", strconv.Itoa(rx))
				resp, err := cl.Do(newreq)
				if err != nil {
					if time.Since(started) >= maxRequestDuration {
						// since request has exceeded our max request timeout
						log.Error(logger, "timed out (max request)", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "rx", rx, "user-agent", req.Header.Get("user-agent"), "err", err)
						ow.WriteHeader(http.StatusGatewayTimeout)
						return
					}
					if err == context.Canceled || strings.Contains(err.Error(), "context canceled") {
						// the client closed connection
						log.Error(logger, "timed out (cancelled)", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "rx", rx, "user-agent", req.Header.Get("user-agent"), "err", err)
						ow.WriteHeader(http.StatusNoContent)
						return
					}
					if err == context.DeadlineExceeded || strings.Contains(err.Error(), "deadline exceeded") {
						// timed out
						log.Error(logger, "timed out (deadline)", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "rx", rx, "user-agent", req.Header.Get("user-agent"), "err", err)
						ow.WriteHeader(http.StatusGatewayTimeout)
						return
					}
					log.Debug(logger, "retryable error, will retry", "err", err, "method", req.Method, "path", req.URL.Path, "retries", i, "delayed", time.Since(started), "user-agent", req.Header.Get("user-agent"))
					time.Sleep(time.Duration(i) * time.Millisecond * time.Duration(10))
					continue
				}
				switch resp.StatusCode {
				case 408, 429, 502, 503, 504:
					io.Copy(ioutil.Discard, resp.Body)
					resp.Body.Close()
					log.Debug(logger, "retryable status, will retry", "status", resp.StatusCode, "method", req.Method, "path", req.URL.Path, "retries", i, "delayed", time.Since(started), "user-agent", req.Header.Get("user-agent"))
					time.Sleep(time.Duration(i) * time.Millisecond * time.Duration(10))
					continue
				}
				// must write the headers *before* the status and body
				for k, v := range resp.Header {
					for _, h := range v {
						ow.Header().Add(k, h)
					}
				}
				ow.WriteHeader(resp.StatusCode)
				tx, _ = io.Copy(ow, resp.Body)
				resp.Body.Close()
				if f, ok := ow.(http.Flusher); ok {
					f.Flush()
				}
				log.Debug(logger, "routed", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "tx", tx, "rx", rx, "user-agent", req.Header.Get("user-agent"), "status", resp.StatusCode)
				return
			}
			// if we get here, we timed out
			ow.WriteHeader(http.StatusServiceUnavailable)
			log.Error(logger, "timed out", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "rx", rx, "user-agent", req.Header.Get("user-agent"))
		})

		port, _ := cmd.Flags().GetInt("port")

		s := &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: redirect,
		}

		// wait for the server to exit and cleanup
		pos.OnExit(func(_ int) {
			log.Info(logger, "shutdown")
			s.Shutdown(context.Background())
		})

		log.Info(logger, "starting HTTP", "port", port, "upstream", upstream)
		if err := s.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(logger, "error starting server", "err", err)
		}

	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	log.RegisterFlags(rootCmd)
	rootCmd.Flags().Int("port", pos.GetenvInt("PORT", 8080), "port to listen")
	rootCmd.Flags().String("upstream", pos.Getenv("UPSTREAM", "http://127.0.0.1:8081"), "upstream server")
	rootCmd.Flags().Bool("echo", false, "if we should just echo back")
	rootCmd.Flags().MarkHidden("echo")
}
