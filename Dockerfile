FROM golang:1.17.6

ENV CGO_ENABLED=0
WORKDIR /workspace
ADD go.mod go.sum ./
RUN go mod download
ADD . .
RUN go build \
		-o geth-gateway \
		-ldflags "-w -s" \
		.

FROM gcr.io/distroless/static

WORKDIR /app

COPY --from=0 /workspace/geth-gateway ./

ENTRYPOINT ["/app/geth-gateway"]
