package db

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"
)

func ConnectRedis() (*redis.Client, error) {
	redisURL := os.Getenv("REDIS_URL")
	var opts *redis.Options
	var err error

	if redisURL != "" {
		opts, err = redis.ParseURL(redisURL)
		if err != nil {
			return nil, err
		}
	} else {
		opts = &redis.Options{
			Addr:     "localhost:6379",
			Password: "",
			DB:       0,
		}
	}

	client := redis.NewClient(opts)

	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}

	return client, nil
}
