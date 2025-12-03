FROM golang:1.24 as build-stage

WORKDIR /app

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/slurm-sidecar cmd/main.go


# Deploy the application binary into a lean image
#FROM ubuntu:latest AS build-release-stage
FROM almalinux:9

# Settings for all images
ENV TIMEZONE=America/New_York

# Install system packages
RUN DEBIAN_FRONTEND=noninteractive \
  dnf install -y epel-release \
  && dnf install -y --allowerasing slurm-slurmd rpm-build slurm-slurmctld apptainer sshpass slirp4netns iproute curl tar xz make gcc ca-certificates \
  && update-ca-trust \
  && dnf clean all

RUN slurm-setuser -y

# Slurm configuration
COPY docker/slurm.conf /etc/slurm/slurm.conf
COPY docker/cgroup.conf /etc/slurm/cgroup.conf
RUN mkdir -p /var/spool/slurmctld
RUN mkdir -p /var/spool/slurmd /var/log/slurm /etc/slurm

# Startup configuration
COPY docker/startup.sh /etc/startup.sh
RUN chmod 555 /etc/startup.sh

# Install MUNGE from source (to get the latest version)
RUN dnf install -y wget tar make gcc openssl-devel libtool autoconf automake \
  && dnf clean all

RUN wget https://github.com/dun/munge/releases/download/munge-0.5.16/munge-0.5.16.tar.xz \
  && tar -xf munge-0.5.16.tar.xz \
  && cd munge-0.5.16 \
  && ./configure --prefix=/ --sysconfdir=/etc/munge --with-munge-user=root --with-munge-group=root \
  && make \
  && make install
RUN ln -s /usr/local/munge/bin/* /usr/local/bin/ && ln -s /usr/local/munge/sbin/* /usr/local/sbin/


WORKDIR /root

COPY --from=build-stage /app/bin/slurm-sidecar /sidecar/slurm-sidecar

ENV SLURMCONFIGPATH=/root/SlurmConfig.yaml

RUN mkdir -p /cvmfs/grid.cern.ch/etc/grid-security

ENTRYPOINT ["/bin/sh", "-c", "/etc/startup.sh && SHARED_FS=true /sidecar/slurm-sidecar "]