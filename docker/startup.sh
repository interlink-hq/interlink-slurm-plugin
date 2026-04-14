#!/bin/bash

# Determine whether script is running as root
sudo_cmd=""
if [ "$(id -u)" != "0" ]; then
  sudo_cmd="sudo"
  sudo -k
fi

# Configure Slurm to use maximum available processors and memory
# and start required services
${sudo_cmd} bash <<SCRIPT
sed -i "s/<<HOSTNAME>>/$(hostname)/" /etc/slurm/slurm.conf
sed -i "s/<<CPU>>/$(nproc)/" /etc/slurm/slurm.conf
sed -i "s/<<MEMORY>>/$(if [[ "$(slurmd -C)" =~ RealMemory=([0-9]+) ]]; then echo "${BASH_REMATCH[1]}"; else exit 100; fi)/" /etc/slurm/slurm.conf
mkdir -p /run/munge
chown munge:munge /run/munge
[ -f /etc/munge/munge.key ] || mungekey --verbose
munged --force
slurmd
slurmctld
SCRIPT

# Wait for slurmctld to be ready before starting sidecar
for i in $(seq 1 30); do
  if scontrol ping 2>/dev/null | grep -q "is UP"; then
    break
  fi
  sleep 1
done

# Revoke sudo permissions
if [[ ${sudo_cmd} ]]; then
  sudo -k
fi
