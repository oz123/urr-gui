OUT_DIR     := out
ARCHS       := arm64 amd64
GUI_SRC     := cmd/glkvm/main.go go.mod Dockerfile cmd/glkvm/templates/index.html cmd/glkvm/static/milligram.css
MNAS_SRC    := cmd/mnas/main.go go.mod Dockerfile
GUI_HOST ?= user@glinet
MNAS_HOST ?= user@gentoo
# override: make install-gui GUI_HOST=oznt@192.168.0.72

.PHONY: all build gui mnas install install-gui install-mnas test clean

all: build

build: gui mnas

# urr-gui (web UI) per arch
gui: $(addprefix $(OUT_DIR)/urr-gui-,$(ARCHS))
$(OUT_DIR)/urr-gui-%: $(GUI_SRC)
	docker buildx build --target output --build-arg GOARCH=$* \
		--build-arg BIN=urr-gui --build-arg PKG=./cmd/glkvm \
		-o $(OUT_DIR) -t urr-gui:$* .
	mv $(OUT_DIR)/urr-gui $(OUT_DIR)/urr-gui-$*
	@echo "Built: $@"
	@file $@

# mnas (suspend service) per arch
mnas: $(addprefix $(OUT_DIR)/mnas-,$(ARCHS))
$(OUT_DIR)/mnas-%: $(MNAS_SRC)
	docker buildx build --target output --build-arg GOARCH=$* \
		--build-arg BIN=mnas --build-arg PKG=./cmd/mnas \
		-o $(OUT_DIR) -t mnas:$* .
	mv $(OUT_DIR)/mnas $(OUT_DIR)/mnas-$*
	@echo "Built: $@"
	@file $@

test:
	go vet ./...
	go test ./...

install: install-gui install-mnas

# GL.iNet box (busybox): binary + S99 hook + env config
install-gui: $(OUT_DIR)/urr-gui-arm64
	scp -O $(OUT_DIR)/urr-gui-arm64 $(GUI_HOST):bin/urr-gui
	scp -O etc/glinet/S99urr-gui $(GUI_HOST):/etc/kvmd/user/scripts/S99urr-gui
	scp -O etc/glinet/urr-gui.env  $(GUI_HOST):/etc/kvmd/user/urr-gui.env
	ssh $(GUI_HOST) 'chmod +x ./bin/urr-gui /etc/kvmd/user/scripts/S99urr-gui'

# Gentoo box (OpenRC): binary + init script + conf (service: mnas-wake-and-suspend)
install-mnas: $(OUT_DIR)/mnas-amd64
	scp $(OUT_DIR)/mnas-amd64             $(MNAS_HOST):/usr/local/bin/mnas-wake-and-suspend
	scp etc/gentoo/init.d/mnas-wake-and-suspend $(MNAS_HOST):/etc/init.d/mnas-wake-and-suspend
	scp etc/gentoo/mnas-wake-and-suspend   $(MNAS_HOST):/etc/conf.d/mnas-wake-and-suspend
	ssh $(MNAS_HOST) 'chmod 755 /usr/local/bin/mnas-wake-and-suspend /etc/init.d/mnas-wake-and-suspend'

clean:
	rm -f $(OUT_DIR)/*
	rmdir $(OUT_DIR) 2>/dev/null || true
