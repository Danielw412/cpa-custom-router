# custom-quota-router: build, test and deploy. See docs/development.md.

export GOTOOLCHAIN ?= go1.26.10
export CGO_ENABLED := 1

MODULE  := github.com/Danielw412/cpa-custom-router
PLUGIN  := custom-quota-router
VERSION ?= 0.0.0
SO      := build/$(PLUGIN)-v$(VERSION).so

# Staging builds for the end-to-end suites (the second one tests hot upgrade).
E2E_SO      := $(CURDIR)/build/$(PLUGIN)-v0.0.1.so
E2E_SO_NEXT := $(CURDIR)/build/$(PLUGIN)-v0.0.2.so

.PHONY: check fmt vet test build mock e2e deploy clean

check: vet test

fmt:
	gofmt -w .

vet:
	go vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

test:
	go test -race -count=1 ./...

# make build VERSION=1.6.0 -> build/custom-quota-router-v1.6.0.so
build:
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X $(MODULE)/internal/plugin.Version=$(VERSION)" -o $(SO) .
	rm -f $(SO:.so=.h)

mock:
	cd test/e2e/mock && go build -o $(CURDIR)/build/cqr-mock .

e2e: mock
	$(MAKE) build VERSION=0.0.1
	$(MAKE) build VERSION=0.0.2
	SO=$(E2E_SO) test/e2e/staging.sh
	SO_NEXT=$(E2E_SO_NEXT) python3 test/e2e/e2e.py || { test/e2e/staging.sh stop; exit 1; }
	test/e2e/staging.sh stop
	SO=$(E2E_SO) test/e2e/staging.sh
	python3 test/e2e/e2e_windows.py || { test/e2e/staging.sh stop; exit 1; }
	test/e2e/staging.sh stop

# make deploy VERSION=1.6.0 (production; read docs/operations.md first)
deploy:
	@if [ "$(VERSION)" = "0.0.0" ]; then echo "set VERSION=x.y.z (higher than the installed one)"; exit 1; fi
	scripts/deploy.py --version $(VERSION)

clean:
	rm -rf build
