package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

var (
	ctx = context.Background()

	redisClient *redis.Client
	db          *sql.DB
)

type UserProfile struct {
	ID            int64  `json:"id"`
	Bio           string `json:"bio"`
	BannerHash    string `json:"banner_hash"`
	AudioHash     string `json:"audio_hash"`
	AudioMimeType string `json:"audio_mime_type"`
	AudioName     string `json:"audio_name"`
}

func getAudioFilename(ctx context.Context, userID, hash string) (string, error) {
	key := "user:profile:" + userID

	jsonStr, err := redisClient.Get(ctx, key).Result()
	if err == nil {
		var profile UserProfile
		if err := json.Unmarshal([]byte(jsonStr), &profile); err == nil {
			if profile.AudioHash == hash && profile.AudioName != "" {
				return profile.AudioName, nil
			}
		}
	} else if err != redis.Nil {
		log.Printf("valkey GET error: %v", err)
	}

	var dbFilename string
	
	err = db.QueryRowContext(ctx,
		`SELECT audio_name FROM user_profiles WHERE id = $1 AND audio_hash = $2`,
		userID, hash).Scan(&dbFilename)

	if err != nil {
		return "", err
	}

	return dbFilename, nil
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, reading config from environment")
	}

	redisClient = redis.NewClient(&redis.Options{
		Addr:     os.Getenv("VALKEY_ADDR"),
		Password: "",
		DB:       0,
	})

	pgConnStr := os.Getenv("POSTGRES_CONN")
	if pgConnStr == "" {
		log.Fatal("POSTGRES_CONN is not set")
	}

	var err error
	db, err = sql.Open("postgres", pgConnStr)
	if err != nil {
		log.Fatalf("failed to open postgres connection: %v", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("failed to ping postgres: %v", err)
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
		switch {
		case strings.HasPrefix(req.URL.Path, "/avatars/"):
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

		case strings.HasPrefix(req.URL.Path, "/banners/"):
			parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/banners/"), "/", 2)
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

				req.URL.Path = "/" + minioBucket + "/banners/" + userID + "/" + hash + "." + format
				req.URL.Scheme = minioURL.Scheme
				req.URL.Host = minioURL.Host
				return
			}

		case strings.HasPrefix(req.URL.Path, "/songs/"):
			parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/songs/"), "/", 2)
			if len(parts) == 2 {
				userID := parts[0]
				hashWithExt := parts[1]

				ext := filepath.Ext(hashWithExt)
				hash := strings.TrimSuffix(hashWithExt, ext)

				req.URL.Path = "/" + minioBucket + "/songs/" + userID + "/" + hash + ext
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

		if strings.HasPrefix(resp.Request.URL.Path, "/" + minioBucket + "/songs/") {
			parts := strings.SplitN(strings.TrimPrefix(resp.Request.URL.Path,  "/" + minioBucket + "/songs/"), "/", 2)
			if len(parts) == 2 {
				userID := parts[0]
				hashWithExt := parts[1]

				ext := filepath.Ext(hashWithExt)
				hash := strings.TrimSuffix(hashWithExt, ext)

				audioName, err := getAudioFilename(ctx, userID, hash)
				if err == nil && audioName != "" {
					resp.Header.Set("Content-Disposition", `inline; filename="`+ audioName +`"`)
				}
			}
		}

		return nil
	}

	log.Printf("starting b2/cdn-proxy on %s\n", listenAddr)

	err = http.ListenAndServe(listenAddr, proxy)
	if err != nil {
		log.Fatal(err)
	}
}
