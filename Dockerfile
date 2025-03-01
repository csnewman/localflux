FROM golang:alpine AS app-builder
WORKDIR /go/src/app
COPY . .
RUN apk add git
RUN CGO_ENABLED=0 go install -ldflags '-extldflags "-static"' -tags timetzdata ./cmd/localflux

FROM scratch
COPY --from=app-builder /go/bin/localflux /
COPY --from=alpine:latest /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["/localflux"]
