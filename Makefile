LDFLAGS=-s -w -X main.version=$(shell git describe --always --dirty 2>/dev/null || echo dev)
all:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/bcvmtools-linux-amd64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS) -H windowsgui" -o dist/bcvmtools-windows-amd64.exe .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/bcvmtools-darwin-arm64 .

# regenerate the Windows manifest resource (only needed if app.manifest changes)
syso:
	go run ./tools/mksyso app.manifest rsrc_windows_amd64.syso
