package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/go-redis/redis/v7"
	"github.com/nanzhong/tester/db"
	testerhttp "github.com/nanzhong/tester/http"
	"github.com/nanzhong/tester/scheduler"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "sere the web UI",
	Args:  cobra.ExactArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		configPath := viper.GetString("serve-config")
		file, err := os.Open(configPath)
		if err != nil {
			if os.IsNotExist(err) {
				log.Fatalf("config (%s) does not exist", configPath)
			}
			log.Fatalf("failed to read config (%s): %s", configPath, err)
		}
		var cfg config
		err = json.NewDecoder(file).Decode(&cfg)
		if err != nil {
			log.Fatalf("failed to parse config (%s): %s", configPath, err)
		}

		l, err := net.Listen("tcp", viper.GetString("serve-addr"))
		if err != nil {
			log.Fatalf("failed to listen on %s", viper.GetString("serve-addr"))
		}

		var dbStore db.DB
		if viper.GetString("serve-redis-url") != "" {
			log.Printf("configuring redis backend")
			dbStore, err = configureRedis()
			if err != nil {
				log.Fatal("failed to configure redis: %w", err)
			}
		} else if viper.GetString("serve-s3-endpoint") != "" {
			log.Printf("configuring s3 backend")
			dbStore = configureS3()
		} else {
			log.Printf("configuring memory backend")
			dbStore = &db.MemDB{}
		}

		scheduler := scheduler.NewScheduler(cfg.Packages, scheduler.WithDB(dbStore))
		uiHandler := testerhttp.NewUIHandler(testerhttp.WithDB(dbStore))
		apiHandler := testerhttp.NewAPIHandler(testerhttp.WithDB(dbStore))

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.Handle("/api/", apiHandler)
		mux.Handle("/", uiHandler)

		httpServer := http.Server{
			Handler: mux,
		}

		done := make(chan os.Signal, 1)
		signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			defer close(done)
			<-done

			log.Println("shutting down")
			{
				// Give one minute for running requests to complete
				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
				defer cancel()

				var eg errgroup.Group
				eg.Go(func() error {
					log.Printf("attempting to shutdown http server")
					return httpServer.Shutdown(ctx)
				})
				eg.Go(func() error {
					log.Printf("attempting to shutdown scheduler")
					scheduler.Stop()
					return nil
				})
				err := eg.Wait()
				if err != nil {
					log.Printf("failed to gracefully shutdown: %s", err)
				}
			}
		}()

		var eg errgroup.Group
		eg.Go(func() error {
			log.Printf("serving on %s", viper.GetString("serve-addr"))
			return httpServer.Serve(l)
		})
		eg.Go(func() error {
			log.Print("starting scheduler")
			scheduler.Run()
			return nil
		})
		eg.Go(func() error {
			ticker := time.NewTicker(15 * time.Second)
			for {
				select {
				case <-done:
					return nil
				case <-ticker.C:
					err := dbStore.Archive(context.Background())
					if err != nil {
						log.Printf("failed to archive results: %w", err)
					}
				}
			}
		})
		err = eg.Wait()
		log.Printf("server ended: %s", err)
	},
}

func init() {
	serveCmd.Flags().String("config", "", "Path to the configuration file")
	viper.BindPFlag("serve-config", serveCmd.Flags().Lookup("config"))

	serveCmd.Flags().String("addr", "0.0.0.0:8080", "The address to serve on")
	viper.BindPFlag("serve-addr", serveCmd.Flags().Lookup("addr"))

	serveCmd.Flags().String("redis-url", "", "The url string of redis")
	viper.BindPFlag("serve-redis-url", serveCmd.Flags().Lookup("redis-url"))

	serveCmd.Flags().String("s3-bucket", "", "The name of the s3 compatible bucket")
	viper.BindPFlag("serve-s3-bucket", serveCmd.Flags().Lookup("s3-bucket"))
	serveCmd.Flags().String("s3-endpoint", "", "The endpoint for a s3 compatible backend")
	viper.BindPFlag("serve-s3-endpoint", serveCmd.Flags().Lookup("s3-endpoint"))
	serveCmd.Flags().String("s3-region", "", "The region for a s3 compatible backend")
	viper.BindPFlag("serve-s3-region", serveCmd.Flags().Lookup("s3-region"))
	serveCmd.Flags().String("s3-key", "", "The s3 access key id")
	viper.BindPFlag("serve-s3-key", serveCmd.Flags().Lookup("s3-key"))
	serveCmd.Flags().String("s3-secret", "", "The s3 secret access key")
	viper.BindPFlag("serve-s3-secret", serveCmd.Flags().Lookup("s3-secret"))
}

func configureRedis() (db.DB, error) {
	redisURL := viper.GetString("serve-redis-url")

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}

	redisClient := redis.NewClient(opt)

	_, err = redisClient.Ping().Result()
	if err != nil {
		return nil, fmt.Errorf("verifying redis connectivity: %w", err)
	}
	return db.NewRedis(redisClient), nil
}

func configureS3() db.DB {
	s3Key := viper.GetString("serve-s3-key")
	s3Secret := viper.GetString("serve-s3-secret")
	s3Endpoint := viper.GetString("serve-s3-endpoint")
	s3Region := viper.GetString("serve-s3-region")
	s3Bucket := viper.GetString("serve-s3-bucket")

	s3Config := &aws.Config{
		Credentials: credentials.NewStaticCredentials(s3Key, s3Secret, ""),
		Endpoint:    &s3Endpoint,
		Region:      &s3Region,
	}
	return db.NewS3(s3Config, s3Bucket)
}
