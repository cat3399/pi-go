#!/usr/bin/env bash
set -euo pipefail

if (( $# != 4 )); then
	printf 'usage: %s <missing|present> <surface|all|csv> <version> <asset-list-file>\n' "$0" >&2
	exit 2
fi

mode=$1
selection=$2
version=$3
asset_file=$4
case "$mode" in
	missing|present) ;;
	*)
		printf 'unsupported mode: %s\n' "$mode" >&2
		exit 2
		;;
esac
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	printf 'version must be stable semantic version, got: %s\n' "$version" >&2
	exit 2
fi
if [[ ! -r "$asset_file" ]]; then
	printf 'asset list does not exist: %s\n' "$asset_file" >&2
	exit 2
fi

assets=()
while IFS= read -r asset; do
	if [[ -n "$asset" ]]; then
		assets+=("$asset")
	fi
done < "$asset_file"

if [[ "$selection" == all ]]; then
	selected=(terminal web gui mobile)
else
	IFS=',' read -r -a requested <<< "$selection"
	selected=()
	seen=,
	for surface in "${requested[@]}"; do
		case "$surface" in
			terminal|web|gui|mobile) ;;
			*)
				printf 'unsupported surface: %s\n' "$surface" >&2
				exit 2
				;;
		esac
		if [[ "$seen" == *",$surface,"* ]]; then
			continue
		fi
		selected+=("$surface")
		seen+="$surface,"
	done
	if (( ${#selected[@]} == 0 )); then
		printf 'surface selection is empty\n' >&2
		exit 2
	fi
fi

asset_exists() {
	local expected=$1 asset
	for asset in "${assets[@]}"; do
		if [[ "$asset" == "$expected" ]]; then
			return 0
		fi
	done
	return 1
}

surface_complete() {
	local surface=$1 target extension
	case "$surface" in
		terminal|web)
			for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
				extension=tar.gz
				if [[ "$target" == windows_* ]]; then
					extension=zip
				fi
				asset_exists "pi-go-${surface}_${version}_${target}.${extension}" || return 1
			done
			;;
		gui)
			for target in linux_amd64 darwin_amd64 darwin_arm64 windows_amd64; do
				extension=tar.gz
				if [[ "$target" == windows_* ]]; then
					extension=zip
				fi
				asset_exists "pi-go-gui_${version}_${target}.${extension}" || return 1
			done
			;;
		mobile)
			asset_exists "pi-go-mobile_${version}_android_arm64.apk" || return 1
			;;
	esac
}

result=()
for surface in "${selected[@]}"; do
	if surface_complete "$surface"; then
		state=present
	else
		state=missing
	fi
	if [[ "$state" == "$mode" ]]; then
		result+=("$surface")
	fi
done

if (( ${#result[@]} > 0 )); then
	(IFS=,; printf '%s\n' "${result[*]}")
fi
