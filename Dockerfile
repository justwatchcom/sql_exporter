FROM golang:1.20-alpine as builder

RUN apk add git bash

ENV GO111MODULE=on

# Add our code
COPY ./ /src

# build
WORKDIR /src
RUN GOGC=off go build -mod=vendor -v -o /sql_exporter .

# multistage
FROM gcr.io/distroless/static:nonroot

USER nonroot:nonroot
COPY --from=bin --chown=nonroot:nonroot --chmod=0755 /usr/bin/sql_exporter /

ENTRYPOINT ["/sql_exporter"]
