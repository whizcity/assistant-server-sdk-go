module github.com/whizcity/assistant-server-sdk-go/auditgorm

go 1.25.14

replace github.com/whizcity/assistant-server-sdk-go => ../

require (
	github.com/jinzhu/gorm v1.9.16
	github.com/mattn/go-sqlite3 v1.14.0
	github.com/whizcity/assistant-server-sdk-go v0.0.0-00010101000000-000000000000
)

require github.com/jinzhu/inflection v1.0.0 // indirect
