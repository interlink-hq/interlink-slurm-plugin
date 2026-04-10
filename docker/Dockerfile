FROM golang:1.24 AS build-stage

WORKDIR /app

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/slurm-sidecar cmd/main.go

# Deploy the application binary into a lean image
#FROM ubuntu:latest AS build-release-stage
FROM almalinux:9

# Settings for all images
ENV TIMEZONE=America/New_York

# Install system packages (without Slurm -- built from source below)
RUN DEBIAN_FRONTEND=noninteractive \
  dnf install -y epel-release \
  && /usr/bin/crb enable \
  && dnf install -y --allowerasing rpm-build apptainer sshpass slirp4netns iproute curl tar xz make gcc ca-certificates \
  wget bzip2 python3 munge-devel pam-devel readline-devel perl-ExtUtils-MakeMaker \
  autoconf automake mariadb-devel libjwt-devel json-c-devel http-parser-devel \
  && update-ca-trust \
  && dnf clean all

# Build and install Slurm 25.11 from source (AlmaLinux 9 ships 22.05 which is
# incompatible with Slinky's slurmctld 25.11 wire protocol)
RUN wget https://download.schedmd.com/slurm/slurm-25.11.4.tar.bz2 \
  && rpmbuild -ta --with jwt --with slurmrestd slurm-25.11.4.tar.bz2 \
  && dnf install -y ~/rpmbuild/RPMS/x86_64/slurm-25.11.4*.rpm \
  ~/rpmbuild/RPMS/x86_64/slurm-slurmd-25.11.4*.rpm \
  ~/rpmbuild/RPMS/x86_64/slurm-slurmctld-25.11.4*.rpm \
  ~/rpmbuild/RPMS/x86_64/slurm-sackd-25.11.4*.rpm \
  && rm -rf ~/rpmbuild slurm-25.11.4.tar.bz2

# Ensure slurm user exists (slurm-setuser is not available in 25.11 RPMs)
RUN id slurm &>/dev/null || useradd -r -s /sbin/nologin slurm

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