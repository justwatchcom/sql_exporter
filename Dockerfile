FROM golang:1.17-alpine as builder

RUN apk add git bash

ENV GO111MODULE=on

# Add our code
COPY ./ /src

# build
WORKDIR /src
RUN GOGC=off go build -mod=vendor -v -o /sql_exporter .

# multistage
FROM alpine:latest

RUN apk --update upgrade && \
    apk add curl ca-certificates && \
    apk add tzdata && \
    update-ca-certificates && \
    rm -rf /var/cache/apk/*

COPY --from=builder /sql_exporter /usr/bin/sql_exporter

# Run the image as a non-root user
RUN adduser -D prom
RUN chmod 0755 /usr/bin/sql_exporter

USER prom

CMD sql_exporter