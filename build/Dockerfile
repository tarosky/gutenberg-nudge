FROM ubuntu:20.04

ARG BCC_VERSION=0.16.0
ARG GO_VERSION=1.15.2

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
  arping \
  bison \
  build-essential \
  cmake \
  flex \
  git \
  iperf3 \
  libclang-9-dev \
  libedit-dev \
  libelf-dev \
  libllvm9 \
  llvm-9-dev \
  netperf \
  python3 \
  python3-distutils \
  zlib1g-dev

RUN cd /var/tmp && git clone -b v${BCC_VERSION} https://github.com/iovisor/bcc.git
RUN mkdir -p /var/tmp/bcc/build && cd /var/tmp/bcc/build

RUN update-alternatives --install /usr/local/bin/python python /usr/bin/python3.8 1

WORKDIR /var/tmp/bcc/build

RUN cmake ..
RUN make
RUN make install

# build python3 binding
RUN cmake -DPYTHON_CMD=python3 ..

WORKDIR /var/tmp/bcc/build/src/python

RUN make
RUN make install

RUN mkdir -p /var/tmp/go
WORKDIR /var/tmp/go
RUN apt-get install -y curl

RUN curl -sSfL https://golang.org/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -C /usr/local -xz
ENV PATH $PATH:/usr/local/go/bin

RUN mkdir /workspace
WORKDIR /workspace
