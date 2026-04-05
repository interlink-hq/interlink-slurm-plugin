# Change tag to switch Slurm version
ARG SLURM_TAG=25.11-ubuntu24.04

FROM golang:1.24 AS build-stage

WORKDIR /app

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/slurm-sidecar cmd/main.go

# Copy sackd from the Slinky sackd image (not included in slurmd)
FROM ghcr.io/slinkyproject/sackd:${SLURM_TAG} AS sackd-source

# Base on official Slinky slurmd image
FROM ghcr.io/slinkyproject/slurmd:${SLURM_TAG}

ENV DEBIAN_FRONTEND=noninteractive

# Install runtime dependencies + Apptainer
RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    software-properties-common sshpass slirp4netns iproute2 curl ca-certificates \
  && add-apt-repository -y ppa:apptainer/ppa \
  && apt-get update \
  && apt-get install -y --no-install-recommends apptainer \
  && update-ca-certificates \
  && rm -rf /var/lib/apt/lists/*

# Copy sackd binary (auth daemon for auth/slurm)
COPY --from=sackd-source /usr/sbin/sackd /usr/sbin/sackd

# Slurm directories
RUN mkdir -p /var/spool/slurmctld /var/spool/slurmd /var/log/slurm /etc/slurm

# Slurm configuration
COPY docker/slurm.conf /etc/slurm/slurm.conf
COPY docker/cgroup.conf /etc/slurm/cgroup.conf

# Startup configuration
COPY docker/startup.sh /etc/startup.sh
RUN chmod 555 /etc/startup.sh

WORKDIR /root

COPY --from=build-stage /app/bin/slurm-sidecar /sidecar/slurm-sidecar

ENV SLURMCONFIGPATH=/root/SlurmConfig.yaml

RUN mkdir -p /cvmfs/grid.cern.ch/etc/grid-security

ENTRYPOINT ["/bin/sh", "-c", "/etc/startup.sh && SHARED_FS=true /sidecar/slurm-sidecar "]
