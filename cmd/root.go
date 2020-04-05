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
	"time"

	"github.com/pinpt/go-common/log"
	pos "github.com/pinpt/go-common/os"
	"github.com/spf13/cobra"
)

const maxRetryDuration = time.Second * 10

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
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true, // since we reuse on each call
		DisableCompression:    true, // disable so we can stream byte-for-byte
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
			var retries int
			ctx, cancel := context.WithTimeout(req.Context(), maxRetryDuration)
			defer cancel()
			for time.Since(started) < maxRetryDuration {
				newreq, _ := http.NewRequestWithContext(ctx, req.Method, u.String(), bytes.NewReader(body))
				for k, v := range req.Header {
					newreq.Header.Set(k, v[0])
				}
				newreq.Header.Set("Content-Length", strconv.Itoa(len(body)))
				cl := newClient() // new client each time so we don't reuse the same connection in failure
				resp, err := cl.Do(newreq)
				if err != nil {
					if err == context.Canceled || strings.Contains(err.Error(), "context canceled") {
						// the client closed connection
						ow.WriteHeader(http.StatusNoContent)
						return
					}
					if err == context.DeadlineExceeded || strings.Contains(err.Error(), "deadline exceeded") {
						// timed out
						ow.WriteHeader(http.StatusGatewayTimeout)
						return
					}
					retries++
					log.Debug(logger, "retryable error, will retry", "err", err, "method", req.Method, "path", req.URL.Path, "retries", retries, "delayed", time.Since(started), "user-agent", req.Header.Get("user-agent"))
					time.Sleep(time.Duration(retries) * time.Millisecond * time.Duration(10))
					continue
				}
				switch resp.StatusCode {
				case 408, 429, 502, 503, 504:
					io.Copy(ioutil.Discard, resp.Body)
					resp.Body.Close()
					retries++
					log.Debug(logger, "retryable status, will retry", "status", resp.StatusCode, "method", req.Method, "path", req.URL.Path, "retries", retries, "delayed", time.Since(started), "user-agent", req.Header.Get("user-agent"))
					time.Sleep(time.Duration(retries) * time.Millisecond * time.Duration(10))
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
				log.Debug(logger, "routed", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "tx", tx, "rx", rx, "user-agent", req.Header.Get("user-agent"), "status", resp.StatusCode)
				return
			}
			// if we get here, we timed out
			ow.WriteHeader(http.StatusServiceUnavailable)
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
