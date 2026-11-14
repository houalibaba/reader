APP_NAME := reader
SERVER_DIR := server
WEB_DIR := web
DIST_DIR := dist

.PHONY: all web build-amd64 build-arm64 test package clean

all: web build-amd64

web:
	cd $(WEB_DIR) && npm install && npm run build

build-amd64:
	mkdir -p $(DIST_DIR)
	cd $(SERVER_DIR) && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags="-s -w -extldflags '-static'" \
		-o ../$(DIST_DIR)/$(APP_NAME)-linux-amd64 ./cmd/reader

build-arm64:
	mkdir -p $(DIST_DIR)
	cd $(SERVER_DIR) && CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		go build -trimpath -ldflags="-s -w -extldflags '-static'" \
		-o ../$(DIST_DIR)/$(APP_NAME)-linux-arm64 ./cmd/reader

test:
	python3 tools/verify_e2e.py
	cd $(SERVER_DIR) && go test -v ./...

package:
	python3 tools/story_packager.py --output $(DIST_DIR)/story_xiaoxiong_v1.zip

clean:
	rm -rf $(DIST_DIR) $(SERVER_DIR)/internal/web/dist/*
