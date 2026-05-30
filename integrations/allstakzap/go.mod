module github.com/AllStak/allstak-go/integrations/allstakzap

go 1.23.4

require (
	github.com/AllStak/allstak-go v0.0.0
	go.uber.org/zap v1.27.0
)

require go.uber.org/multierr v1.10.0 // indirect

replace github.com/AllStak/allstak-go => ../..
