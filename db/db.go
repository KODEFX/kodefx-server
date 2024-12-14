package db

import (
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func NewPSQLStorage() (*gorm.DB, error) {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: No .env file found, relying on environment variables")
	}

	connString := os.Getenv("DB_URL")
	if connString == "" {
		log.Fatal("DB_URL is not set in the environment variables")
	}

	// Connect to the database
	db, err := gorm.Open(postgres.Open(connString), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// Configure connection pooling
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(50)                 // Maximum number of open connections
	sqlDB.SetMaxIdleConns(25)                 // Maximum number of idle connections
	sqlDB.SetConnMaxLifetime(30 * time.Minute) // Maximum lifetime of a connection
	sqlDB.SetConnMaxIdleTime(10 * time.Minute) // Maximum idle time of a connection

	return db, nil
}
