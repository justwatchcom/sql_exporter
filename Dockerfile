FROM        quay.io/prometheus/busybox:glibc
MAINTAINER  6RS DevOps Team <eng_sw_devops@6river.com>

COPY ./sql_exporter /bin/sql_exporter

EXPOSE      9237
ENTRYPOINT  [ "/bin/sql_exporter" ]
