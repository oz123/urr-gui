FROM golang:1.26-alpine AS builder
ARG GOARCH=arm64
ARG BIN=urr-gui
# PKG is the Go package to build (module root is ".", mnas is "./cmd/mnas")
ARG PKG=.
WORKDIR /app
COPY go.mod ./
COPY . .
# Cross-compile natively; the binary links statically so no emulation needed
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH \
    go build -ldflags="-s -w" -o /out/$BIN $PKG

FROM scratch AS output
COPY --from=builder /out/$BIN /$BIN
