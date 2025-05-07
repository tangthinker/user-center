package db

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"sync"
)

var (
	db       *gorm.DB
	once     sync.Once
	rootPath string
)

func GetDB() *gorm.DB {
	once.Do(func() {
		path := "user-center.db"
		if rootPath != "" {
			path = rootPath + "/" + path
		}
		DB, err := gorm.Open(sqlite.Open(path), &gorm.Config{})

		if err != nil {
			panic(err)
		}

		db = DB
	})

	return db
}

func SetDBPath(path string) {
	rootPath = path
}
