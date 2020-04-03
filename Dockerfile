FROM golang:alpine as builder
RUN apk add git
RUN go get -u github.com/golang/dep/...
WORKDIR /go/src/github.com/pinpt/retry-proxy
ADD . .
RUN dep ensure --vendor-only
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-extldflags "-static"' -o main .
FROM scratch
COPY --from=builder /go/src/github.com/pinpt/retry-proxy/main /app/
WORKDIR /app
CMD ["./main"]