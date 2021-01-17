module github.com/tarosky/gutenberg-nudge

go 1.15

require (
	github.com/tarosky/gutenberg-notifier v0.0.0-20210117224941-11fc8a03a832
	github.com/urfave/cli/v2 v2.2.0
	go.uber.org/zap v1.16.0
)

replace github.com/iovisor/gobpf => github.com/harai/gobpf v0.0.0-20200830051040-3869641b1144
