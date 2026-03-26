FROM golang:1.24-bookworm AS builder

ARG VERSION_FLAGS=""

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    make \
    gcc \
    libc-dev \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go install -ldflags "${VERSION_FLAGS}" ./cmd/evm-gateway

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

ADD https://github.com/CosmWasm/wasmvm/releases/download/v2.1.5/libwasmvm.x86_64.so /lib/libwasmvm.x86_64.so
ADD https://github.com/CosmWasm/wasmvm/releases/download/v2.1.5/libwasmvm.aarch64.so /lib/libwasmvm.aarch64.so

COPY --from=builder /go/bin/evm-gateway /usr/local/bin/evm-gateway

WORKDIR /apps/data

CMD ["evm-gateway"]
