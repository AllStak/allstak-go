module github.com/allstak/allstak-go/integrations/allstakgorm

go 1.23.4

require (
	github.com/AllStak/allstak-go v0.0.0
	gorm.io/gorm v1.25.12
)

require (
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-sqlite3 v1.14.22 // indirect
	golang.org/x/text v0.14.0 // indirect
	gorm.io/driver/sqlite v1.5.7 // indirect
)

replace github.com/AllStak/allstak-go => ../..
