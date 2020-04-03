package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/pinpt/go-common/log"
	pos "github.com/pinpt/go-common/os"
	"github.com/spf13/cobra"
)

const maxAttempts = time.Second * 10

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use: "retry-proxy",
	Run: func(cmd *cobra.Command, args []string) {
		logger := log.NewCommandLogger(cmd)
		defer logger.Close()

		upstream, _ := cmd.Flags().GetString("upstream")
		_, err := url.Parse(upstream)

		if err != nil {
			log.Fatal(logger, "error parsing upstream url", "err", err, "url", upstream)
		}

		redirect := http.HandlerFunc(func(ow http.ResponseWriter, req *http.Request) {
			started := time.Now()
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				ow.WriteHeader(http.StatusBadRequest)
				return
			}
			u, _ := url.Parse(upstream)
			u.Path = req.URL.Path
			var tx int64
			rx := len(body)
			var retries int
			for time.Since(started) < maxAttempts {
				req, _ := http.NewRequestWithContext(req.Context(), req.Method, u.String(), bytes.NewReader(body))
				for k, v := range req.Header {
					req.Header.Set(k, v[0])
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					retries++
					log.Debug(logger, "retryable status, will retry", "err", err, "method", req.Method, "path", req.URL.Path, "retries", retries, "delayed", time.Since(started))
					time.Sleep(time.Duration(retries) * time.Millisecond * time.Duration(10))
					continue
				}
				switch resp.StatusCode {
				case 429, 499, 502, 503, 504:
					io.Copy(ioutil.Discard, resp.Body)
					resp.Body.Close()
					retries++
					log.Debug(logger, "retryable status, will retry", "status", resp.StatusCode, "method", req.Method, "path", req.URL.Path, "retries", retries, "delayed", time.Since(started))
					time.Sleep(time.Duration(retries) * time.Millisecond * time.Duration(10))
					continue
				}
				for k, v := range resp.Header {
					req.Header.Set(k, v[0])
				}
				tx, _ = io.Copy(ow, resp.Body)
				resp.Body.Close()
				log.Debug(logger, "routed", "path", req.URL.Path, "method", req.Method, "duration", time.Since(started), "tx", tx, "rx", rx)
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
}
