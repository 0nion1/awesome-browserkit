.PHONY: install test race smoke build demo lab snapshot-lab pack cli server
install:
	go mod download
	npm ci --prefix react
	npm run build --prefix react
	npm ci --prefix examples/demo/web

test:
	go test ./...
	npm test --prefix react

race:
	go test -race ./...

smoke:
	go test -tags=browser_smoke . ./cmd/browserkit -run 'TestRealBrowserOwnsAndRemovesLocalProfile|TestRealBrowserProfileIdentityMatchesRequests|TestRealBrowserManagerWorkflow|TestRealBrowserNetworkPolicy|TestRealBrowserFunctionToolLab|TestRealBrowserEmbeddedTargets|TestRealBrowserSnapshotBehaviorLab|TestRealBrowserSequenceGenerateAndReplay|TestRealBrowserLoadingTimeoutReturnsCurrentContent|TestRealCLIModes' -count=1 -v

build:
	go build ./...
	npm run build --prefix react
	npm run build --prefix examples/demo/web

cli:
	go build -o browserkit ./cmd/browserkit

server:
	go build -o browserkit-server ./cmd/browserkit-server

demo:
	go run ./examples/demo

lab:
	go run ./examples/tool-lab

snapshot-lab:
	go run ./examples/tool-lab --addr 127.0.0.1:8879

pack:
	cd react && npm pack
