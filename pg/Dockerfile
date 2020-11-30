# Inspired by https://www.cloudreach.com/blog/containerize-this-golang-dockerfiles/

# Build inside a Go container.
FROM golang:1.14-stretch as builder

ENV GOPATH /build
ENV CGO_ENABLED 0

COPY . $GOPATH/src/entrogo.com/entroq
WORKDIR $GOPATH/src/entrogo.com/entroq

RUN apt-get install -y git
RUN go get -d -v ./... && go install -v ./...

# Also build parallel from source, embed in a script, and then we can make our command from that.
RUN mkdir -p /tmp/src \
 && cd /tmp/src \
 && apt-get update \
 && apt-get install -y curl bzip2 make jq \
 && curl -LO https://ftp.gnu.org/gnu/parallel/parallel-20201022.tar.bz2 \
 && tar xjf parallel-20201022.tar.bz2 \
 && cd parallel-20201022 \
 && ./configure \
 && make \
 && make install

# Create a GNU parallel script that can run both postgres and eqsvc
# simultaneously, configured to stop everything if either one goes down
# (--halt).
#
# The docker-entrypoint.sh script comes from the postgres base image.
# The eqsvc.sh script comes from our builder base image.
#
# Note that we cut out the last 6 lines of the embedded parallel script: these
# are noisy example invocations, so we don't need them.
RUN parallel --embed | head -n -6 > eq_and_pg.sh \
 && echo 'parallel --halt now,done=1 ::: "docker-entrypoint.sh postgres" "eqsvc.sh pg --port=37706"' >> eq_and_pg.sh \
 && chmod +x eq_and_pg.sh \
 && mv eq_and_pg.sh /build/bin/



# Switch to the Postgres container - based on stretch. We'll copy
# Our tool binaries into this one and change the entrypoint.
FROM postgres:11.1

RUN mkdir -p /go/bin \
 && mkdir -p $HOME/.parallel \
 && touch $HOME/.parallel/will-cite

COPY --from=builder /build/bin/* /go/bin/
ENV PATH ${PATH}:/go/bin

COPY cmd/eqsvc.sh /go/bin/
WORKDIR /go/bin

USER postgres

# gRPC endpoint
EXPOSE 37706

# Prometheus endpoint
EXPOSE 9100

# PostgreSQL endpoint
EXPOSE 5432

VOLUME /var/lib/postgresql/data


CMD ["eq_and_pg.sh"]
