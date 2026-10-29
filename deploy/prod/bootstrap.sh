#!/usr/bin/env bash
# Prepares a fresh Debian 12/13 VPS for the CLEADS stack. Idempotent: safe to
# run again. Run as root (sudo):
#
#   sudo DEPLOY_PUBKEY="ssh-ed25519 AAAA... deploy" ./bootstrap.sh
#   sudo HARDEN_SSH=1 ./bootstrap.sh     # once key login is verified
#
# What it does: Docker (official repository), a "deploy" user (docker group,
# key-only), firewall (22, 80, 443), fail2ban, unattended security upgrades,
# nightly backups, /opt/cleads.
set -euo pipefail

[ "$(id -u)" = 0 ] || { echo "run as root: sudo $0" >&2; exit 1; }
export DEBIAN_FRONTEND=noninteractive
APP_DIR=/opt/cleads

log() { printf '\n==> %s\n' "$*"; }

log "System packages"
apt-get update -y
apt-get install -y --no-install-recommends ca-certificates curl gnupg ufw fail2ban python3-systemd \
  unattended-upgrades cron rsync openssl

log "Docker"
if ! command -v docker >/dev/null 2>&1; then
  # shellcheck source=/dev/null
  . /etc/os-release
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${ID} ${VERSION_CODENAME} stable" \
    >/etc/apt/sources.list.d/docker.list
  apt-get update -y
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
cat >/etc/docker/daemon.json <<'JSON'
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "20m", "max-file": "5" },
  "live-restore": true
}
JSON
systemctl enable --now docker
systemctl reload docker || systemctl restart docker

log "deploy user"
id deploy >/dev/null 2>&1 || useradd -m -s /bin/bash deploy
usermod -aG docker deploy
install -d -o deploy -g deploy -m 700 /home/deploy/.ssh
if [ -n "${DEPLOY_PUBKEY:-}" ]; then
  touch /home/deploy/.ssh/authorized_keys
  grep -qxF "$DEPLOY_PUBKEY" /home/deploy/.ssh/authorized_keys || echo "$DEPLOY_PUBKEY" >>/home/deploy/.ssh/authorized_keys
  chown deploy:deploy /home/deploy/.ssh/authorized_keys
  chmod 600 /home/deploy/.ssh/authorized_keys
fi
install -d -o deploy -g deploy -m 750 "$APP_DIR" "$APP_DIR/backups"

log "Firewall"
# Docker publishes ports through iptables and bypasses ufw: the compose file
# only publishes 80/443 publicly and binds everything else to 127.0.0.1.
ufw default deny incoming
ufw default allow outgoing
ufw limit 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 443/udp
ufw --force enable

log "fail2ban (sshd)"
cat >/etc/fail2ban/jail.d/cleads-sshd.local <<'EOF'
[sshd]
enabled = true
backend = systemd
maxretry = 5
findtime = 10m
bantime = 1h
EOF
systemctl enable --now fail2ban
systemctl restart fail2ban

log "Unattended security upgrades"
cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF

log "Nightly database backup (03:15 UTC, 7 days kept)"
cat >/etc/cron.d/cleads-backup <<EOF
15 3 * * * deploy $APP_DIR/backup.sh 2>&1 | logger -t cleads-backup
EOF
chmod 644 /etc/cron.d/cleads-backup

if [ "${HARDEN_SSH:-0}" = 1 ]; then
  log "SSH hardening (key only, no root login)"
  # Named 00-* because sshd keeps the FIRST value it reads and cloud images
  # ship 50-cloud-init.conf with PasswordAuthentication yes.
  cat >/etc/ssh/sshd_config.d/00-cleads.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
MaxAuthTries 3
EOF
  sshd -t
  systemctl reload ssh
  sshd -T | grep -Ei '^(passwordauthentication|permitrootlogin|kbdinteractiveauthentication) '
fi

log "Done"
docker --version
docker compose version
ufw status | head -12
