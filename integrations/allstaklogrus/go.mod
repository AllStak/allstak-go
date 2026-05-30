module github.com/AllStak/allstak-go/integrations/allstaklogrus

go 1.23.4

require (
	github.com/AllStak/allstak-go v0.0.0
	github.com/sirupsen/logrus v1.9.3
)

require golang.org/x/sys v0.0.0-20220715151400-c0bba94af5f8 // indirect

replace github.com/AllStak/allstak-go => ../..
