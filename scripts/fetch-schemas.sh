#!/usr/bin/env bash
#
# fetch-schemas.sh — fetch, verify, and re-vendor the OCPP JSON schemas under
# schemas/v201, schemas/v16/base and schemas/v16/security.
#
# The schemas in this repository are copied byte-for-byte from the Open
# Charge Alliance's published OCPP 2.0.1 and OCPP 1.6 archives (see
# schemas/NOTICE for full provenance). This script is how that vendoring is
# verified and, if ever necessary, redone from scratch: the tree it produces
# is expected to be identical to what is already committed.
#
# Usage:
#   fetch-schemas.sh [--check|--verify-remote|--refresh] [--help]
#
# Modes:
#   --check          (default) Verify the vendored schemas/ tree against
#                     schemas/SHA256SUMS, in both directions: every listed
#                     file must exist with the listed hash, and every .json
#                     file under schemas/ must be listed. Also checks the
#                     per-set counts (128 / 56 / 22) and that the tree is
#                     what it claims to be — plain files in the four
#                     expected directories, no symbolic links, no nesting.
#                     Reads only the local filesystem; makes no network
#                     connection.
#
#   --verify-remote   Download OCA's current archives to a temporary
#                     directory, extract them, and compare every freshly
#                     downloaded schema file against the file already
#                     vendored in schemas/. Reports any file that differs,
#                     is missing on either side, or whose count does not
#                     match the expected 128 / 56 / 22. Makes no change to
#                     the repository.
#
#   --refresh         Download OCA's current archives, extract them, and
#                     replace schemas/v201, schemas/v16/base and
#                     schemas/v16/security with the freshly downloaded
#                     files, regenerating schemas/SHA256SUMS. This is the
#                     only mode that writes inside schemas/. The new tree
#                     and its manifest are assembled and fully verified in a
#                     staging directory beside schemas/, and only then
#                     exchanged with the live tree by rename, so a download,
#                     disk or permission failure leaves the existing
#                     schemas/ exactly as it was instead of half
#                     overwritten. The tree being replaced is moved into a
#                     .schemas-backup.XXXXXX directory beside schemas/ for
#                     the duration of that exchange, and is removed only
#                     once the new tree is in place and has verified there.
#                     If anything goes wrong the backup directory is kept
#                     and its path printed, so the previous tree can always
#                     be put back by hand. A run that is killed outright can
#                     therefore leave a .schemas-backup.* or a
#                     .schemas-refresh.* directory at the repository root;
#                     both sit outside schemas/, so nothing that checks the
#                     vendored tree is affected by them, and both can simply
#                     be deleted once their contents are no longer wanted.
#                     Use --refresh to rebuild the vendored tree from
#                     nothing, or to pick up a genuine OCA re-publication
#                     (after checking the resulting diff by hand — these
#                     files are supposed to be static).
#
# Both the download URLs below and the format of schemas/SHA256SUMS are
# reproduced in schemas/NOTICE; if a URL below has moved, re-resolve it from
# the OCA download page: https://www.openchargealliance.org/my-oca/ocpp/
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SCHEMAS_DIR="${REPO_ROOT}/schemas"
MANIFEST="${SCHEMAS_DIR}/SHA256SUMS"

LANDING_PAGE="https://www.openchargealliance.org/my-oca/ocpp/"
USER_AGENT="Mozilla/5.0 (compatible; fetch-schemas.sh; +https://github.com/enesismail/ocpp-go)"

B1_URL="https://openchargealliance.org/wp-content/uploads/downloads/OCPP-2.0.1_all_files-12.zip"
B2_URL="https://openchargealliance.org/wp-content/uploads/2025/04/OCPP_1.6_documentation.zip"
B3_URL="https://openchargealliance.org/wp-content/uploads/downloads/OCPP-1.6-security-whitepaper-edition-4.zip"

EXPECTED_V201=128
EXPECTED_V16_BASE=56
EXPECTED_V16_SECURITY=22

MODE="check"

# Set directories exchanged during a refresh, in the order they are exchanged.
# Recorded here so the rollback path knows exactly what to put back.
SWAPPED_SETS=""

# Scratch directories a refresh creates: the download/extract area and the
# staging tree. Both hold nothing but freshly downloaded material, so both are
# thrown away on every exit path.
WORK_DIR=""
STAGE_ROOT=""

# BACKUP_DIR holds the live schemas/ sets and manifest while they are being
# exchanged for the freshly downloaded ones. For as long as it is set it holds
# the only copy of those files, so nothing removes it on the way out: it is
# created beside schemas/ rather than inside the staging tree precisely so that
# the cleanup below cannot reach it. It is emptied in exactly two places, both
# of which have first established that a second copy exists — after the new
# tree is in place and has verified there, and after a failed exchange has been
# rolled back in full. Anywhere else, the path is printed and the directory
# left alone for a human to deal with.
BACKUP_DIR=""

# clean_up runs on every exit. It removes the scratch directories and, if the
# backup still holds the previous tree, says where that tree is. This is the
# single place the surviving-backup message comes from, so a run cannot both
# report a preserved backup and quietly delete it.
clean_up() {
	local dir
	for dir in "${WORK_DIR}" "${STAGE_ROOT}"; do
		if [ -n "${dir}" ] && [ -d "${dir}" ]; then
			chmod -R u+rwX "${dir}" 2>/dev/null || true
			rm -rf "${dir}"
		fi
	done
	if [ -n "${BACKUP_DIR}" ] && [ -d "${BACKUP_DIR}" ]; then
		echo "the previous contents of schemas/ have been kept in ${BACKUP_DIR}" >&2
		echo "nothing else will remove that directory: move the files back by hand once the cause is understood" >&2
	fi
}
trap clean_up EXIT

usage() {
	sed -n '2,63p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
	case "$1" in
	--check)
		MODE="check"
		;;
	--verify-remote)
		MODE="verify-remote"
		;;
	--refresh)
		MODE="refresh"
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		usage >&2
		exit 1
		;;
	esac
	shift
done

# sha256_tool echoes the name of the sha256 utility to use: sha256sum where
# available (typical on Linux), falling back to shasum -a 256 (typical on
# macOS). Both accept -b/--binary and both can verify a "*"-prefixed
# SHA256SUMS file produced by the other.
sha256_tool() {
	if command -v sha256sum >/dev/null 2>&1; then
		echo "sha256sum"
	elif command -v shasum >/dev/null 2>&1; then
		echo "shasum -a 256"
	else
		echo "error: neither sha256sum nor shasum is available on this system" >&2
		exit 1
	fi
}

sha256_of() {
	local tool
	tool="$(sha256_tool)"
	$tool "$1" | awk '{print $1}'
}

# generate_manifest writes a SHA256SUMS-format manifest covering every .json
# file under the three vendored set directories of the tree rooted at $1, one
# line per file, sorted by path under C collation, using binary-mode "*"
# entries. Paths are recorded relative to that root, so the manifest produced
# for a staging tree is identical to the one produced for the repository. This
# is the one and only manifest generator: schemas/SHA256SUMS in the repository
# and the --refresh mode below both come from this function, so there is no
# way for the manifest format to drift between the two.
generate_manifest() {
	local base="$1" out="$2" tool
	tool="$(sha256_tool)"
	(
		cd "${base}"
		LC_ALL=C find schemas/v201 schemas/v16/base schemas/v16/security -type f -name '*.json' | LC_ALL=C sort
	) | (cd "${base}" && xargs $tool -b) >"${out}"
}

# manifest_paths prints the path recorded on every entry of a SHA256SUMS file,
# one per line, sorted, with the binary-mode "*" marker stripped. Both the
# GNU ("hash *path" / "hash  path") and the shasum spellings are accepted.
manifest_paths() {
	LC_ALL=C sed -E -e '/^[[:space:]]*$/d' -e 's/^[0-9a-fA-F]{64} [ *]?//' "$1" | LC_ALL=C sort
}

fail_download() {
	local url="$1"
	echo "error: failed to download ${url}" >&2
	echo "if this URL has moved, re-resolve it from the OCA download page: ${LANDING_PAGE}" >&2
	exit 1
}

download() {
	local url="$1" dest="$2"
	if ! curl -fsSL -A "${USER_AGENT}" -o "${dest}" "${url}"; then
		fail_download "${url}"
	fi
}

# inventory_and_copy takes stock of one freshly extracted OCA schema directory
# and copies it into $2. Every entry it holds, at any depth, must be a plain,
# visible .json file sitting directly in the directory; anything else stops
# the run for a human to look at. A "*.json" shell glob would have been
# shorter, but it silently skips a dot-prefixed file, silently ignores a stray
# sub-directory or archive, and (with nullglob set) silently copies nothing at
# all — three ways to end up with a quietly incomplete vendored corpus. The
# copied file count is compared against the inventory before the caller is
# allowed to use the result.
inventory_and_copy() {
	local source="$1" destination="$2" label="$3"
	local entry relative base found=0 copied

	if [ ! -d "${source}" ]; then
		echo "error: ${label}: expected schema directory ${source} does not exist" >&2
		return 1
	fi
	mkdir -p "${destination}"

	while IFS= read -r -d '' entry; do
		relative="${entry#"${source}/"}"
		base="$(basename "${entry}")"
		if [ -L "${entry}" ]; then
			echo "error: ${label}: ${relative} is a symbolic link; only plain files are vendored" >&2
			return 1
		fi
		if [ ! -f "${entry}" ]; then
			echo "error: ${label}: ${relative} is not a regular file" >&2
			echo "the bundle layout may have changed; stop and investigate before re-vendoring" >&2
			return 1
		fi
		case "${relative}" in
		*/*)
			echo "error: ${label}: ${relative} sits in a sub-directory; the schema sets are flat" >&2
			return 1
			;;
		esac
		case "${base}" in
		.* | _*)
			echo "error: ${label}: ${relative} starts with '.' or '_'; such names are invisible to shell globs and to file embedding" >&2
			return 1
			;;
		esac
		case "${base}" in
		*.json) ;;
		*)
			echo "error: ${label}: ${relative} is not a .json file; only schema files are vendored" >&2
			echo "the bundle layout may have changed; stop and investigate before re-vendoring" >&2
			return 1
			;;
		esac
		cp "${entry}" "${destination}/${base}"
		found=$((found + 1))
	done < <(find "${source}" -mindepth 1 -print0)

	if [ "${found}" -eq 0 ]; then
		echo "error: ${label}: no schema files were found in the downloaded bundle" >&2
		return 1
	fi
	copied="$(find "${destination}" -mindepth 1 -maxdepth 1 -type f -name '*.json' | wc -l | tr -d ' ')"
	if [ "${copied}" != "${found}" ]; then
		echo "error: ${label}: ${copied} of ${found} schema files arrived in ${destination}" >&2
		return 1
	fi
	return 0
}

# fetch_and_extract downloads all three OCA bundles into the working directory
# $1 and copies the three flat sets of .json files they contain into the three
# destination directories given as $2, $3 and $4:
#   $2 <- B1's nested OCPP-2.0.1_part3_JSON_schemas.zip
#   $3 <- B2's schemas/json/ directory
#   $4 <- B3's nested JSON_schemas.zip
fetch_and_extract() {
	local work="$1" dest_v201="$2" dest_v16base="$3" dest_v16sec="$4"

	download "${B1_URL}" "${work}/B1.zip"
	download "${B2_URL}" "${work}/B2.zip"
	download "${B3_URL}" "${work}/B3.zip"

	mkdir -p "${work}/b1" "${work}/b2" "${work}/b3"
	unzip -q "${work}/B1.zip" -d "${work}/b1"
	unzip -q "${work}/B2.zip" -d "${work}/b2"
	unzip -q "${work}/B3.zip" -d "${work}/b3"

	local b1_nested b3_nested
	b1_nested="$(find "${work}/b1" -name 'OCPP-2.0.1_part3_JSON_schemas.zip')"
	b3_nested="$(find "${work}/b3" -name 'JSON_schemas.zip')"
	if [ -z "${b1_nested}" ]; then
		echo "error: could not find OCPP-2.0.1_part3_JSON_schemas.zip inside the downloaded 2.0.1 bundle" >&2
		echo "the bundle layout may have changed; re-check ${LANDING_PAGE}" >&2
		exit 1
	fi
	if [ -z "${b3_nested}" ]; then
		echo "error: could not find JSON_schemas.zip inside the downloaded 1.6 security whitepaper bundle" >&2
		echo "the bundle layout may have changed; re-check ${LANDING_PAGE}" >&2
		exit 1
	fi

	unzip -q "${b1_nested}" -d "${work}/v201-extract"
	unzip -q "${b3_nested}" -d "${work}/v16-sec-extract"

	local v201_src v16sec_src v16base_src
	v201_src="$(find "${work}/v201-extract" -mindepth 1 -maxdepth 1 -type d)"
	v16sec_src="$(find "${work}/v16-sec-extract" -mindepth 1 -maxdepth 1 -type d)"
	v16base_src="$(find "${work}/b2" -type d -path '*/schemas/json')"

	if [ -z "${v201_src}" ] || [ -z "${v16sec_src}" ] || [ -z "${v16base_src}" ]; then
		echo "error: could not locate the expected schema directory inside a downloaded bundle" >&2
		echo "the bundle layout may have changed; re-check ${LANDING_PAGE}" >&2
		exit 1
	fi

	inventory_and_copy "${v201_src}" "${dest_v201}" "the OCPP 2.0.1 part 3 schema archive"
	inventory_and_copy "${v16base_src}" "${dest_v16base}" "the OCPP 1.6 documentation archive"
	inventory_and_copy "${v16sec_src}" "${dest_v16sec}" "the OCPP 1.6 security whitepaper archive"
}

check_count() {
	local dir="$1" expected="$2" label="$3" got
	got="$(find "${dir}" -maxdepth 1 -type f -name '*.json' | wc -l | tr -d ' ')"
	if [ "${got}" != "${expected}" ]; then
		echo "error: ${label} file count: got ${got}, want ${expected}" >&2
		echo "either a file was added or removed here, or OCA has re-issued the bundle; stop and investigate before re-vendoring" >&2
		return 1
	fi
	return 0
}

# verify_tree checks one schemas/ tree in full and is the single definition of
# what "verified" means: it is run against the repository by --check, and
# against the staging tree by --refresh before anything is exchanged.
#
# The tree is rooted at $1 (which holds schemas/, whose manifest is
# schemas/SHA256SUMS with paths relative to $1), and the checks are:
#   * every file listed in the manifest exists and hashes as listed;
#   * every .json file present under schemas/ is listed in the manifest —
#     without this direction an extra, unlisted file rides along unverified;
#   * nothing under schemas/ is a symbolic link — a link hashes and diffs
#     exactly like the file it points at, while quietly ending the property
#     the corpus exists for, namely that copying schemas/ copies real bytes;
#   * the tree is flat and holds nothing unexpected: the four known
#     directories, the .json files directly inside the three sets, and the
#     NOTICE / SHA256SUMS / guard-test files at the schemas/ root;
#   * the per-set file counts are 128 / 56 / 22.
verify_tree() {
	local base="$1"
	local schemas="${base}/schemas"
	local manifest="${schemas}/SHA256SUMS"
	local tool status=0
	tool="$(sha256_tool)"

	if [ ! -d "${schemas}" ]; then
		echo "error: ${schemas} not found" >&2
		return 1
	fi
	if [ ! -f "${manifest}" ]; then
		echo "error: ${manifest} not found" >&2
		return 1
	fi

	if [ -L "${schemas}" ]; then
		echo "error: schemas is a symbolic link; the vendored corpus must be a real directory of real files" >&2
		status=1
	fi

	local entry relative accepted
	while IFS= read -r -d '' entry; do
		relative="${entry#"${schemas}/"}"
		if [ -L "${entry}" ]; then
			echo "error: symbolic link under schemas/: ${relative}" >&2
			status=1
			continue
		fi
		if [ -d "${entry}" ]; then
			case "${relative}" in
			v201 | v16 | v16/base | v16/security) ;;
			*)
				echo "error: unexpected directory under schemas/: ${relative} (the tree holds only v201, v16, v16/base and v16/security)" >&2
				status=1
				;;
			esac
			continue
		fi
		if [ ! -f "${entry}" ]; then
			echo "error: not a regular file under schemas/: ${relative}" >&2
			status=1
			continue
		fi
		case "${relative}" in
		NOTICE | SHA256SUMS | schemas_test.go)
			continue
			;;
		esac
		accepted=0
		case "${relative}" in
		v201/*/* | v16/base/*/* | v16/security/*/*)
			echo "error: schema file below a set directory: ${relative} (the three sets are flat)" >&2
			status=1
			continue
			;;
		v201/*.json | v16/base/*.json | v16/security/*.json)
			accepted=1
			;;
		esac
		if [ "${accepted}" -ne 1 ]; then
			echo "error: unexpected file under schemas/: ${relative} (only .json files inside v201, v16/base and v16/security, plus NOTICE, SHA256SUMS and schemas_test.go at the root)" >&2
			status=1
		fi
	done < <(find "${schemas}" -mindepth 1 -print0)

	if ! (cd "${base}" && $tool -c "schemas/SHA256SUMS"); then
		status=1
	fi

	local listed_paths present_paths unlisted
	listed_paths="$(manifest_paths "${manifest}")"
	present_paths="$(cd "${base}" && LC_ALL=C find schemas -type f -name '*.json' | LC_ALL=C sort)"
	if [ -n "${present_paths}" ]; then
		unlisted="$(printf '%s\n' "${present_paths}" | LC_ALL=C grep -Fxv -f <(printf '%s\n' "${listed_paths}") || true)"
		if [ -n "${unlisted}" ]; then
			while IFS= read -r relative; do
				echo "error: schema file is not listed in schemas/SHA256SUMS: ${relative}" >&2
			done <<<"${unlisted}"
			status=1
		fi
	fi

	check_count "${schemas}/v201" "${EXPECTED_V201}" "schemas/v201" || status=1
	check_count "${schemas}/v16/base" "${EXPECTED_V16_BASE}" "schemas/v16/base" || status=1
	check_count "${schemas}/v16/security" "${EXPECTED_V16_SECURITY}" "schemas/v16/security" || status=1

	return "${status}"
}

# restore_live_tree puts back everything swap_in_staged_tree has exchanged so
# far, using the copies it moved aside into $1. It is the reason a refresh
# that fails halfway leaves schemas/ as it was rather than half new.
#
# Every step is checked, because this is the one path where an ignored failure
# costs files: the live directory is already gone by the time the rollback runs,
# and the copy in $1 is all there is. It returns success only when the live tree
# is demonstrably whole again — every recorded set put back, the manifest put
# back, and not one regular file left behind in the backup. Anything short of
# that is reported as a failure so the caller can stop and keep the backup,
# rather than reporting a tidy failure over an untidy tree.
restore_live_tree() {
	local backup="$1" relative failed=0 leftover

	for relative in ${SWAPPED_SETS}; do
		# rm -rf succeeds on a path that is not there, which is exactly the
		# state left behind when the rename into place is what failed.
		if ! rm -rf "${SCHEMAS_DIR:?}/${relative}"; then
			echo "error: putting schemas/ back: could not clear schemas/${relative}" >&2
			failed=1
			continue
		fi
		if [ -d "${backup}/schemas/${relative}" ] &&
			! mv "${backup}/schemas/${relative}" "${SCHEMAS_DIR}/${relative}"; then
			echo "error: putting schemas/ back: could not restore schemas/${relative}" >&2
			failed=1
		fi
	done

	if [ -f "${backup}/schemas/SHA256SUMS" ] &&
		! mv -f "${backup}/schemas/SHA256SUMS" "${MANIFEST}"; then
		echo "error: putting schemas/ back: could not restore schemas/SHA256SUMS" >&2
		failed=1
	fi

	if [ "${failed}" -ne 0 ]; then
		return 1
	fi

	# Belt and braces: a set that was moved aside but never recorded would be
	# invisible to the loop above and would then be deleted as redundant.
	leftover="$(find "${backup}" -type f -print -quit)"
	if [ -n "${leftover}" ]; then
		echo "error: putting schemas/ back: ${leftover} was moved aside but never restored" >&2
		return 1
	fi

	SWAPPED_SETS=""
	return 0
}

# swap_in_staged_tree exchanges the live schemas/ sets and manifest with the
# fully verified staged ones under $1. Each set is exchanged by moving the live
# directory aside into BACKUP_DIR and renaming the staged directory into its
# place: two renames within one filesystem, which is the smallest window a
# directory replacement can be reduced to on POSIX, and neither of which can
# copy half a file. The manifest goes last, so a tree and a manifest are never
# both half exchanged.
#
# The backup is a fresh directory beside schemas/, not a corner of the staging
# tree, so that the exit cleanup — which exists to throw the staging tree away —
# cannot take the only copy of the live files with it.
#
# It returns:
#   0  the staged tree is in place; BACKUP_DIR holds the previous one
#   1  the exchange failed and the previous tree is back, whole, in schemas/
#   2  the exchange failed and could not be undone; schemas/ is incomplete and
#      BACKUP_DIR holds what is missing from it
swap_in_staged_tree() {
	local staged_root="$1" relative
	local staged_schemas="${staged_root}/schemas"

	if ! BACKUP_DIR="$(mktemp -d "${REPO_ROOT}/.schemas-backup.XXXXXX")"; then
		BACKUP_DIR=""
		echo "error: could not create a directory beside schemas/ to hold the tree being replaced" >&2
		return 1
	fi
	if ! mkdir -p "${BACKUP_DIR}/schemas/v16"; then
		echo "error: could not prepare ${BACKUP_DIR}" >&2
		return 1
	fi
	SWAPPED_SETS=""

	if [ -f "${MANIFEST}" ] && ! mv "${MANIFEST}" "${BACKUP_DIR}/schemas/SHA256SUMS"; then
		echo "error: could not move the existing schemas/SHA256SUMS aside" >&2
		restore_live_tree "${BACKUP_DIR}" || return 2
		return 1
	fi

	for relative in v201 v16/base v16/security; do
		mkdir -p "$(dirname "${SCHEMAS_DIR}/${relative}")"
		if [ -e "${SCHEMAS_DIR}/${relative}" ]; then
			if ! mv "${SCHEMAS_DIR}/${relative}" "${BACKUP_DIR}/schemas/${relative}"; then
				echo "error: could not move the existing schemas/${relative} aside" >&2
				restore_live_tree "${BACKUP_DIR}" || return 2
				return 1
			fi
		fi
		# From here on this set counts as exchanged even if the rename below
		# fails: the live directory is already gone and only the rollback can
		# bring it back.
		SWAPPED_SETS="${SWAPPED_SETS} ${relative}"
		if ! mv "${staged_schemas}/${relative}" "${SCHEMAS_DIR}/${relative}"; then
			echo "error: could not move the freshly downloaded schemas/${relative} into place" >&2
			restore_live_tree "${BACKUP_DIR}" || return 2
			return 1
		fi
	done

	if ! mv "${staged_schemas}/SHA256SUMS" "${MANIFEST}"; then
		echo "error: could not move the regenerated schemas/SHA256SUMS into place" >&2
		restore_live_tree "${BACKUP_DIR}" || return 2
		return 1
	fi

	SWAPPED_SETS=""
	return 0
}

# discard_backup removes the copy of the previous tree and stops the exit
# cleanup announcing it. Both callers have already established that the files
# it holds exist somewhere else as well; a backup that cannot be removed is
# therefore a tidiness problem, not a data one, and is only reported.
discard_backup() {
	if [ -n "${BACKUP_DIR}" ] && [ -d "${BACKUP_DIR}" ] && ! rm -rf "${BACKUP_DIR}"; then
		echo "note: ${BACKUP_DIR} is no longer needed but could not be removed; delete it by hand" >&2
	fi
	BACKUP_DIR=""
}

mode_check() {
	if [ ! -f "${MANIFEST}" ]; then
		echo "error: ${MANIFEST} not found" >&2
		exit 1
	fi
	if ! verify_tree "${REPO_ROOT}"; then
		exit 1
	fi
	echo "schemas/ verifies against schemas/SHA256SUMS: every listed file matches and no unlisted file is present"
}

mode_verify_remote() {
	local work
	WORK_DIR="$(mktemp -d)"
	work="${WORK_DIR}"

	fetch_and_extract "${work}" "${work}/v201" "${work}/v16-base" "${work}/v16-sec"

	local status=0
	check_count "${work}/v201" "${EXPECTED_V201}" "schemas/v201" || status=1
	check_count "${work}/v16-base" "${EXPECTED_V16_BASE}" "schemas/v16/base" || status=1
	check_count "${work}/v16-sec" "${EXPECTED_V16_SECURITY}" "schemas/v16/security" || status=1

	compare_dirs() {
		local fresh="$1" vendored="$2" label="$3"
		local f name fresh_hash vendored_hash
		for f in "${fresh}"/*.json; do
			name="$(basename "${f}")"
			if [ ! -f "${vendored}/${name}" ]; then
				echo "difference: ${label}/${name} was downloaded but is not vendored" >&2
				status=1
				continue
			fi
			fresh_hash="$(sha256_of "${f}")"
			vendored_hash="$(sha256_of "${vendored}/${name}")"
			if [ "${fresh_hash}" != "${vendored_hash}" ]; then
				echo "difference: ${label}/${name} does not match the freshly downloaded copy" >&2
				status=1
			fi
		done
		for f in "${vendored}"/*.json; do
			name="$(basename "${f}")"
			if [ ! -f "${fresh}/${name}" ]; then
				echo "difference: ${label}/${name} is vendored but was not present in the fresh download" >&2
				status=1
			fi
		done
	}

	compare_dirs "${work}/v201" "${SCHEMAS_DIR}/v201" "schemas/v201"
	compare_dirs "${work}/v16-base" "${SCHEMAS_DIR}/v16/base" "schemas/v16/base"
	compare_dirs "${work}/v16-sec" "${SCHEMAS_DIR}/v16/security" "schemas/v16/security"

	if [ "${status}" -eq 0 ]; then
		echo "schemas/ matches a fresh download from OCA: zero differences"
	fi
	return "${status}"
}

mode_refresh() {
	local work stage_root stage_schemas swap_status=0

	WORK_DIR="$(mktemp -d)"
	work="${WORK_DIR}"
	# The staging tree is created beside schemas/ rather than in the system
	# temporary directory, so that the final exchange is a rename within one
	# filesystem instead of a copy that can stop halfway. It holds nothing but
	# freshly downloaded material, so it is removed on every exit path,
	# including a failed or interrupted run.
	STAGE_ROOT="$(mktemp -d "${REPO_ROOT}/.schemas-refresh.XXXXXX")"
	stage_root="${STAGE_ROOT}"

	stage_schemas="${stage_root}/schemas"
	mkdir -p "${stage_schemas}/v16"

	# Everything below writes into the staging tree only. schemas/ is not
	# touched until the staged tree is complete and has verified.
	fetch_and_extract "${work}" \
		"${stage_schemas}/v201" \
		"${stage_schemas}/v16/base" \
		"${stage_schemas}/v16/security"

	generate_manifest "${stage_root}" "${stage_schemas}/SHA256SUMS"

	if ! verify_tree "${stage_root}"; then
		echo "error: the freshly downloaded tree did not verify; schemas/ has been left untouched" >&2
		exit 1
	fi

	swap_in_staged_tree "${stage_root}" || swap_status=$?
	case "${swap_status}" in
	0) ;;
	1)
		echo "error: schemas/ could not be replaced; the previous tree has been left in place" >&2
		discard_backup
		exit 1
		;;
	*)
		# schemas/ is incomplete and the backup is the only copy of what is
		# missing. Stop here and let the exit cleanup print where it is; do
		# not touch it further.
		echo "error: schemas/ could not be replaced and could not be put back; it is now incomplete" >&2
		exit 1
		;;
	esac

	# The staged tree verified in the staging directory; verifying it again
	# where it now lives is what makes the copy of the previous tree safe to
	# throw away. Until this passes, that copy stays on disk.
	if ! verify_tree "${REPO_ROOT}"; then
		echo "error: schemas/ did not verify after the exchange" >&2
		exit 1
	fi

	discard_backup
	echo "schemas/ refreshed from OCA and schemas/SHA256SUMS regenerated"
}

case "${MODE}" in
check)
	mode_check
	;;
verify-remote)
	mode_verify_remote
	;;
refresh)
	mode_refresh
	;;
esac
