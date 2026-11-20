APP_NAME := reader
SERVER_DIR := server
WEB_DIR := web
DIST_DIR := dist
VERSION ?= v0.3.15

# Target baseline x86-64 (GOAMD64=v1, no AVX/AVX2/BMI1/BMI2/FMA/LZCNT) so binaries compiled on
# AMD Ryzen / modern Xeon hosts run without SIGILL (invalid opcode) on Intel Atom x5-Z8350 (Cherry Trail).
AMD64_CGO_CFLAGS := -O2 -march=x86-64 -mtune=generic -mno-avx -mno-avx2 -mno-bmi -mno-bmi2 -mno-fma -mno-lzcnt
AMD64_CGO_LDFLAGS := -march=x86-64
LDFLAGS := -s -w -extldflags '-static' -X reader/server/internal/updater.CurrentVersion=$(VERSION)

.PHONY: all web runtime-bundle build-amd64 build-arm64 test package clean

all: web build-amd64

web:
	cd $(WEB_DIR) && npm install && npm run build

runtime-bundle:
	mkdir -p $(DIST_DIR)
	python3 tools/build_runtime_bundle.py --version "$(VERSION)" --commit "$$(git rev-parse HEAD)" --archive $(DIST_DIR)/reader-runtime.tar.gz --go-output server/internal/updater/runtime_payload_gen.go

build-amd64: runtime-bundle
	mkdir -p $(DIST_DIR)
	cd $(SERVER_DIR) && \
		CFLAGS="" CXXFLAGS="" \
		GOAMD64=v1 CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
		CGO_CFLAGS="$(AMD64_CGO_CFLAGS)" \
		CGO_LDFLAGS="$(AMD64_CGO_LDFLAGS)" \
		go build -trimpath -ldflags="$(LDFLAGS)" \
		-o ../$(DIST_DIR)/$(APP_NAME)-linux-amd64 ./cmd/reader

build-arm64: runtime-bundle
	mkdir -p $(DIST_DIR)
	cd $(SERVER_DIR) && \
		CFLAGS="" CXXFLAGS="" \
		CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		go build -trimpath -ldflags="$(LDFLAGS)" \
		-o ../$(DIST_DIR)/$(APP_NAME)-linux-arm64 ./cmd/reader

test: runtime-bundle web
	PYTHONPATH=tools python3 tools/verify_e2e.py
	cd $(SERVER_DIR) && go mod tidy && go test -v ./...

package:
	python3 tools/story_packager.py --output $(DIST_DIR)/story_xiaoxiong_v1.zip

clean:
	rm -rf $(DIST_DIR) $(SERVER_DIR)/internal/web/dist/*
