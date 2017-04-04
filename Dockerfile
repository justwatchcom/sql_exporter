FROM prom/busybox:glibc

ADD sql_exporter /bin/sql_exporter

EXPOSE 9237
ENTRYPOINT [ "/bin/sql_exporter" ]
