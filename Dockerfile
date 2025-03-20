FROM golang:1.23 AS builder

LABEL org.opencontainers.image.source=https://github.com/wille/haprovider

ARG TARGETOS
ARG TARGETARCH

WORKDIR /haprovider

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o haprovider cmd/haprovider/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /haprovider/haprovider .

ENTRYPOINT ["/haprovider"]
