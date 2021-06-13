module github.com/tarosky/gutenberg-nudge

go 1.15

require (
	github.com/tarosky/gutenberg-notifier v0.0.0-20210612121334-ffdf44029d22
	github.com/urfave/cli/v2 v2.2.0
	go.uber.org/zap v1.16.0
	golang.org/x/sync v0.0.0-20190423024810-112230192c58
)

replace github.com/iovisor/gobpf => github.com/harai/gobpf v0.2.1
