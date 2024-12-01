package db

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var db *gorm.DB

func init() {

	DB, err := gorm.Open(sqlite.Open("user-center.db"), &gorm.Config{})

	if err != nil {
		panic(err)
	}

	db = DB
}

func GetDB() *gorm.DB {
	return db
}
