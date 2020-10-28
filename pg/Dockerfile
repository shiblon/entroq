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
 && apt-get install -y curl bzip2 make \
 && curl -LO https://ftp.gnu.org/gnu/parallel/parallel-20201022.tar.bz2 \
 && tar xjf parallel-20201022.tar.bz2 \
 && cd parallel-20201022 \
 && ./configure \
 && make \
 && make install

RUN parallel --embed | head -n -6 > eqpluspg.sh \
 && echo 'parallel --halt now,done=1 ::: "docker-entrypoint.sh postgres" "eqsvc.sh pg --port=37706"' >> eqpluspg.sh \
 && chmod +x eqpluspg.sh \
 && mv eqpluspg.sh /build/bin/



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

EXPOSE 37706
VOLUME /var/lib/postgresql/data


# TODO: fix entrypoint to call postgres, tini, etc.
CMD ["eqpluspg.sh"]
