#!/usr/bin/env bash
# loon-agent first-run setup.
#
# Writes .env and picks where /data lives. That second part is the whole reason
# this exists: the pipeline stages entire releases to disk, the compose default
# puts that on Docker's data-root (usually the OS disk), and nothing warns you.
# A production agent ran for months staging Blu-ray remuxes onto a 318G root
# filesystem while a 26T array sat unused, because df inside the container
# faithfully reported the disk it had actually been given.
#
# Re-runnable: every prompt defaults to your current value.

set -euo pipefail

readonly ENV_FILE=".env"
readonly OVERRIDE_FILE="docker-compose.override.yml"
readonly DIR_NAME="loon-agent"

if [[ -t 1 ]]; then
  B=$'\033[1m'; DIM=$'\033[2m'; R=$'\033[0m'
  GRN=$'\033[32m'; YEL=$'\033[33m'; RED=$'\033[31m'; CYA=$'\033[36m'
else
  B=""; DIM=""; R=""; GRN=""; YEL=""; RED=""; CYA=""
fi

die()  { echo "${RED}error:${R} $*" >&2; exit 1; }
warn() { echo "${YEL}warning:${R} $*" >&2; }
info() { echo "${DIM}$*${R}"; }
head2() { echo; echo "${B}$*${R}"; }

# ── prompts ────────────────────────────────────────────────────────────────
# ask VAR "Question" "default"
ask() {
  local __var=$1 prompt=$2 default=${3:-} reply
  if [[ -n "$default" ]]; then
    read -r -p "$prompt ${DIM}[$default]${R}: " reply || true
    reply=${reply:-$default}
  else
    read -r -p "$prompt: " reply || true
  fi
  printf -v "$__var" '%s' "$reply"
}

# ask_secret VAR "Question" "current"  — never echoes, keeps current on empty
ask_secret() {
  local __var=$1 prompt=$2 current=${3:-} reply hint=""
  [[ -n "$current" ]] && hint=" ${DIM}[keep existing]${R}"
  read -r -s -p "$prompt$hint: " reply || true
  echo
  [[ -z "$reply" && -n "$current" ]] && reply=$current
  printf -v "$__var" '%s' "$reply"
}

ask_yn() { # ask_yn "Question" "y|n" -> returns 0 for yes
  local prompt=$1 default=${2:-n} reply
  local hint="[y/N]"; [[ $default == y ]] && hint="[Y/n]"
  read -r -p "$prompt ${DIM}$hint${R}: " reply || true
  reply=${reply:-$default}
  [[ ${reply,,} == y* ]]
}

# Read a key out of an existing .env so re-runs default to current values.
# Must return non-zero when it has nothing, or `$(cur X || echo default)` sees
# a successful empty result and the default never fires.
cur() {
  [[ -f $ENV_FILE ]] || return 1
  local v
  v=$(sed -n "s/^$1=//p" "$ENV_FILE" | tail -1)
  [[ -n $v ]] || return 1
  printf '%s' "$v"
}

human_kb() { # KB -> human, for display only
  local kb=$1
  if   (( kb >= 1073741824 )); then printf '%.1fT' "$(bc -l <<<"$kb/1073741824")"
  elif (( kb >= 1048576 ))    ; then printf '%.0fG' "$(bc -l <<<"$kb/1048576")"
  else printf '%.0fM' "$(bc -l <<<"$kb/1024")"
  fi
}

# ── preflight ──────────────────────────────────────────────────────────────
command -v docker >/dev/null || die "docker is not installed."
docker compose version >/dev/null 2>&1 \
  || die "'docker compose' is unavailable. Install the Compose v2 plugin."
[[ -f docker-compose.yml ]] \
  || die "run this from the loon-agent directory (no docker-compose.yml here)."
command -v bc >/dev/null || die "bc is required (apt install bc)."

echo "${B}loon-agent setup${R}"
info "Every prompt defaults to your current value — safe to re-run."

# ── 1. disks ───────────────────────────────────────────────────────────────
head2 "1. Where should downloads and staging live?"

# Real block devices only: tmpfs/overlay/squashfs are not somewhere to put 30GB.
# Sorted by free space, largest first — the answer is nearly always the array.
mapfile -t FS < <(
  df -PT -x tmpfs -x devtmpfs -x overlay -x squashfs -x iso9660 2>/dev/null \
    | awk 'NR>1 && $1 ~ /^\/dev\// { print $7"\t"$5"\t"$3"\t"$2"\t"$1 }' \
    | while IFS=$'\t' read -r mnt rest; do
        # Must be a directory we could actually put a release in. df happily
        # reports single-file bind mounts (/etc/hosts inside a container) and
        # system mounts as filesystems; you cannot mkdir inside a file, and
        # nobody wants staging on /boot.
        [[ -d $mnt ]] || continue
        case $mnt in
          /boot|/boot/*|/dev|/dev/*|/proc|/proc/*|/sys|/sys/*|/run|/run/*|/etc/*) continue ;;
        esac
        printf '%s\t%s\n' "$mnt" "$rest"
      done \
    | sort -k2 -rn
)
(( ${#FS[@]} )) || die "found no usable filesystems. Mount your storage first, then re-run."

printf "\n   %-3s %-24s %10s %10s %-9s %s\n" "#" "MOUNT" "FREE" "SIZE" "TYPE" "DEVICE"
i=0
for row in "${FS[@]}"; do
  IFS=$'\t' read -r mnt avail size type dev <<<"$row"
  i=$((i+1))
  mark=" "
  (( i == 1 )) && mark="${GRN}*${R}"
  printf " %s %-3s %-24s %10s %10s %-9s %s\n" \
    "$mark" "$i)" "$mnt" "$(human_kb "$avail")" "$(human_kb "$size")" "$type" "$dev"
done
echo "   ${DIM}${GRN}*${R}${DIM} = most free space${R}"

DEFAULT_PICK=1
# If /data is already configured, default to whatever holds it.
if cur_data=$(cur AGENT_DATA_DIR) && [[ -n ${cur_data:-} ]]; then
  cur_mnt=$(df -P "$cur_data" 2>/dev/null | awk 'NR==2{print $6}' || true)
  for n in "${!FS[@]}"; do
    [[ ${FS[$n]%%$'\t'*} == "$cur_mnt" ]] && DEFAULT_PICK=$((n+1)) && break
  done
fi

while :; do
  ask PICK "Pick a disk by number" "$DEFAULT_PICK"
  [[ $PICK =~ ^[0-9]+$ ]] && (( PICK >= 1 && PICK <= ${#FS[@]} )) && break
  warn "enter a number between 1 and ${#FS[@]}"
done
IFS=$'\t' read -r DATA_MNT DATA_AVAIL _ _ _ <<<"${FS[$((PICK-1))]}"

# Default to a directory on the disk just chosen. Only re-offer the configured
# path if it actually lives on that disk: otherwise picking a new disk and
# pressing enter would silently keep the old one — the precise failure this
# script exists to prevent.
DATA_DIR_DEFAULT="${DATA_MNT%/}/$DIR_NAME"
if [[ -n ${cur_data:-} ]]; then
  cur_data_mnt=$(df -P "$cur_data" 2>/dev/null | awk 'NR==2{print $6}' || true)
  [[ $cur_data_mnt == "$DATA_MNT" ]] && DATA_DIR_DEFAULT="$cur_data"
fi
ask DATA_DIR "Directory for agent data" "$DATA_DIR_DEFAULT"

# The mkdir is the point of the exercise — do it now so a bad path fails here
# rather than at 3am mid-release.
if ! mkdir -p "$DATA_DIR" 2>/dev/null; then
  die "cannot create $DATA_DIR (try: sudo mkdir -p '$DATA_DIR' && sudo chown $(id -un) '$DATA_DIR')"
fi
[[ -w $DATA_DIR ]] || die "$DATA_DIR is not writable by $(id -un)."
echo "   ${GRN}ok${R} $DATA_DIR ($(human_kb "$DATA_AVAIL") free)"

# ── 2. optional second disk for staging ────────────────────────────────────
head2 "2. Separate disk for staging? ${DIM}(optional)${R}"
info "Staging is the write-heavy path: every release is assembled and par2'd"
info "there before upload. Put it on a fast disk and keep the array for output."

TEMP_DIR=""
if ask_yn "Use a different disk for staging" n; then
  while :; do
    ask TPICK "Pick a disk by number" "1"
    [[ $TPICK =~ ^[0-9]+$ ]] && (( TPICK >= 1 && TPICK <= ${#FS[@]} )) && break
    warn "enter a number between 1 and ${#FS[@]}"
  done
  IFS=$'\t' read -r TEMP_MNT TEMP_AVAIL _ _ _ <<<"${FS[$((TPICK-1))]}"
  ask TEMP_DIR "Directory for staging" "${TEMP_MNT%/}/$DIR_NAME-temp"
  mkdir -p "$TEMP_DIR" 2>/dev/null || die "cannot create $TEMP_DIR (may need sudo)"
  [[ -w $TEMP_DIR ]] || die "$TEMP_DIR is not writable by $(id -un)."
  echo "   ${GRN}ok${R} $TEMP_DIR ($(human_kb "$TEMP_AVAIL") free)"
fi

# ── 3. disk budget ─────────────────────────────────────────────────────────
head2 "3. Disk budget"
budget_mnt_avail=$DATA_AVAIL
[[ -n $TEMP_DIR ]] && budget_mnt_avail=${TEMP_AVAIL:-$DATA_AVAIL}
suggested=$(( budget_mnt_avail / 1048576 * 80 / 100 ))   # 80% of free, in GB
# 0 means "unlimited" to the agent, so a suggestion that rounds down to 0 on a
# small disk would invert into the least conservative setting there is.
(( suggested < 1 )) && suggested=1
info "0 = no limit. The agent uses the LESSER of this and actual free space,"
info "so a budget larger than the disk does nothing."
ask MAX_DISK "Max disk to use, GB (${suggested}G is 80% of free)" "$(cur MAX_DISK_USAGE_GB || echo "$suggested")"

# ── 4. site + key ──────────────────────────────────────────────────────────
head2 "4. ameNZB connection"
ask SITE_URL "Site URL" "$(cur SITE_URL || echo 'https://amenzb.moe')"
info "Your agent key comes from the site: Settings -> Agent -> Reset key."
ask_secret AGENT_TOKEN "Agent key" "$(cur AGENT_TOKEN || true)"
[[ -n $AGENT_TOKEN ]] || die "an agent key is required."

# ── 5. usenet ──────────────────────────────────────────────────────────────
head2 "5. Usenet provider"
ask NNTP_HOST "NNTP host" "$(cur NNTP_HOST || true)"
ask NNTP_PORT "NNTP port" "$(cur NNTP_PORT || echo 563)"
ask NNTP_USER "NNTP username" "$(cur NNTP_USER || true)"
ask_secret NNTP_PASS "NNTP password" "$(cur NNTP_PASS || true)"
ask NNTP_CONNS "Max connections" "$(cur NNTP_CONNECTIONS || echo 20)"
NNTP_SSL=$(cur NNTP_SSL || echo true)
[[ $NNTP_PORT == 119 ]] && NNTP_SSL=false

# ── write ──────────────────────────────────────────────────────────────────
head2 "Summary"
printf "   %-16s %s\n" "data"     "$DATA_DIR"
[[ -n $TEMP_DIR ]] && printf "   %-16s %s\n" "staging" "$TEMP_DIR"
printf "   %-16s %s\n" "disk budget" "$( [[ $MAX_DISK == 0 ]] && echo "unlimited" || echo "${MAX_DISK}G" )"
printf "   %-16s %s\n" "site"     "$SITE_URL"
printf "   %-16s %s\n" "usenet"   "${NNTP_USER}@${NNTP_HOST}:${NNTP_PORT} (ssl=$NNTP_SSL, $NNTP_CONNS conns)"
echo
ask_yn "Write $ENV_FILE and start the agent" y || { echo "Aborted; nothing written."; exit 0; }

if [[ -f $ENV_FILE ]]; then
  backup="$ENV_FILE.bak.$(date +%Y%m%d%H%M%S)"
  cp "$ENV_FILE" "$backup"
  info "backed up existing $ENV_FILE -> $backup"
fi

# Keys this script owns. Everything else in an existing .env — VPN_PROVIDER,
# WIREGUARD_PRIVATE_KEY, OBFUSCATE, whatever the operator added — is theirs and
# must survive: this script is not the only author of the file, and rewriting it
# wholesale would silently drop the VPN config on any existing install.
readonly MANAGED=(
  AGENT_DATA_DIR MAX_DISK_USAGE_GB
  SITE_URL AGENT_TOKEN
  NNTP_HOST NNTP_PORT NNTP_USER NNTP_PASS NNTP_SSL NNTP_CONNECTIONS
)
preserved=""
if [[ -f $ENV_FILE ]]; then
  pattern=$(IFS='|'; printf '%s' "${MANAGED[*]}")
  # Drop our keys and the header we wrote last time; keep everything else as-is.
  preserved=$(grep -vE "^[[:space:]]*($pattern)=" "$ENV_FILE" \
              | grep -v '^# Written by setup.sh' || true)
fi

# 600 before writing: the file holds the agent key and NNTP password, and a
# leaked NNTP credential is what this project has already had to rotate once.
umask 077
{
  echo "# Written by setup.sh on $(date -Iseconds). Re-run setup.sh to change."
  echo
  echo "# Where downloads, staging, par2 and output live (mounted at /data)."
  echo "AGENT_DATA_DIR=$DATA_DIR"
  echo "MAX_DISK_USAGE_GB=$MAX_DISK"
  echo
  echo "SITE_URL=$SITE_URL"
  echo "AGENT_TOKEN=$AGENT_TOKEN"
  echo
  echo "NNTP_HOST=$NNTP_HOST"
  echo "NNTP_PORT=$NNTP_PORT"
  echo "NNTP_USER=$NNTP_USER"
  echo "NNTP_PASS=$NNTP_PASS"
  echo "NNTP_SSL=$NNTP_SSL"
  echo "NNTP_CONNECTIONS=$NNTP_CONNS"
  if [[ -n ${preserved//[[:space:]]/} ]]; then
    echo
    echo "# ── your other settings, preserved ─────────────────────────────"
    printf '%s\n' "$preserved"
  fi
} > "$ENV_FILE"
chmod 600 "$ENV_FILE"
echo "   ${GRN}ok${R} wrote $ENV_FILE (mode 600)"
[[ -n ${preserved//[[:space:]]/} ]] \
  && info "kept $(printf '%s' "$preserved" | grep -c '^[A-Z_]*=' || true) other setting(s) from your previous $ENV_FILE"

# A second disk needs a second bind mount, which compose can't express
# conditionally — so generate an override only when it's actually wanted.
if [[ -n $TEMP_DIR ]]; then
  cat > "$OVERRIDE_FILE" <<EOF
# Written by setup.sh: staging on a separate disk from /data.
services:
  amenzb-agent:
    volumes:
      - $TEMP_DIR:/data/temp
EOF
  echo "   ${GRN}ok${R} wrote $OVERRIDE_FILE (staging bind)"
elif [[ -f $OVERRIDE_FILE ]] && grep -q "Written by setup.sh" "$OVERRIDE_FILE"; then
  rm -f "$OVERRIDE_FILE"
  info "removed previous staging override (staging now lives under /data)"
fi

docker compose config --quiet || die "compose config is invalid — .env was written, fix the above and run: docker compose up -d"

head2 "Starting"
docker compose up -d

echo
echo "   ${GRN}Agent started.${R}"
echo "   ${CYA}docker compose logs -f amenzb-agent${R}   follow the logs"
echo "   ${CYA}docker exec amenzb-agent df -h /data${R}   confirm the disk it got"
echo
info "Check that last one now — it should show the disk you picked above."
