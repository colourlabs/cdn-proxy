package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, reading config from environment")
	}

	minioURLStr := os.Getenv("MINIO_ENDPOINT")
	if minioURLStr == "" {
		log.Fatalf("MINIO_ENDPOINT is not set")
	}

	minioBucket := os.Getenv("MINIO_BUCKET")
	if minioBucket == "" {
		log.Fatalf("MINIO_BUCKET is not set")
	}

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":5000"
	}

	minioURL, err := url.Parse(minioURLStr + "/" + minioBucket)
	if err != nil {
		log.Fatalf("invalid MINIO_ENDPOINT: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(minioURL)
	originalDirector := proxy.Director

	proxy.Director = func(req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/avatars/") {
			parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/avatars/"), "/", 2)
			
			if len(parts) == 2 {
				userID := parts[0]
				hash := parts[1]

				q := req.URL.Query()
				format := q.Get("format")
				if format == "" {
					format = "webp"
				}
				q.Del("format")
				req.URL.RawQuery = q.Encode()

				req.URL.Path = "/" + minioBucket + "/avatars/" + userID + "/" + hash + "." + format

				req.URL.Scheme = minioURL.Scheme
				req.URL.Host = minioURL.Host

				return
			}
		}

		originalDirector(req)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "application/xml") {
			origBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			resp.Body.Close()

			reBucket := regexp.MustCompile(`<BucketName>.*?</BucketName>`)
			reResource := regexp.MustCompile(`<Resource>.*?</Resource>`)
			reKey := regexp.MustCompile(`<Key>.*?</Key>`)

			cleanBody := reBucket.ReplaceAll(origBody, []byte{})
			cleanBody = reResource.ReplaceAll(cleanBody, []byte{})
			cleanBody = reKey.ReplaceAll(cleanBody, []byte{})

			resp.Body = io.NopCloser(bytes.NewReader(cleanBody))
			resp.ContentLength = int64(len(cleanBody))
			resp.Header.Set("Content-Length", strconv.Itoa(len(cleanBody)))
		}
		return nil
	}

	log.Printf("starting b2/cdn-proxy on %s\n", listenAddr)

	err = http.ListenAndServe(listenAddr, proxy)
	if err != nil {
		log.Fatal(err)
	}
}
