#!/usr/bin/env bash
set -euo pipefail

: "${CLEAN_BRANCH:=main}"
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_WORKSPACE:?GITHUB_WORKSPACE is required}"
: "${UPSTREAM_REPOSITORY:=hereww/codextools}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

upstream_default="$(gh repo view "${UPSTREAM_REPOSITORY}" --json defaultBranchRef --jq .defaultBranchRef.name)"
upstream_sha="$(gh api "repos/${UPSTREAM_REPOSITORY}/git/ref/heads/${upstream_default}" --jq .object.sha)"
upstream_tree="$(gh api "repos/${UPSTREAM_REPOSITORY}/git/commits/${upstream_sha}" --jq .tree.sha)"

if gh api "repos/${UPSTREAM_REPOSITORY}/contents/.gitignore?ref=${upstream_sha}" --jq .content > "${tmp_dir}/gitignore.b64" 2>/dev/null; then
  tr -d '\n' < "${tmp_dir}/gitignore.b64" | base64 -d > "${tmp_dir}/gitignore"
else
  : > "${tmp_dir}/gitignore"
fi

if ! grep -qxF "/docs/releases/" "${tmp_dir}/gitignore"; then
  {
    printf "\n"
    printf "# Keep packaged release artifacts out of the cleaned fork.\n"
    printf "/docs/releases/\n"
  } >> "${tmp_dir}/gitignore"
fi

gh api "repos/${UPSTREAM_REPOSITORY}/git/trees/${upstream_tree}?recursive=1" > "${tmp_dir}/upstream-tree.json"
jq '[.tree[]
  | select((((.path | startswith("docs/releases/")) and .type != "tree") or .path == ".github/workflows/deploy-pages.yml"))
  | {path: .path, mode: .mode, type: .type, sha: null}
]' "${tmp_dir}/upstream-tree.json" > "${tmp_dir}/delete-paths.json"

new_tree="$(jq -n \
  --arg base_tree "${upstream_tree}" \
  --slurpfile delete_paths "${tmp_dir}/delete-paths.json" \
  --rawfile gitignore "${tmp_dir}/gitignore" \
  --rawfile workflow "${GITHUB_WORKSPACE}/.github/workflows/sync-upstream.yml" \
  --rawfile script "${GITHUB_WORKSPACE}/.github/scripts/sync-cleaned-fork.sh" \
  '{base_tree: $base_tree, tree: ($delete_paths[0] + [
    {path: ".gitignore", mode: "100644", type: "blob", content: $gitignore},
    {path: ".github/workflows/sync-upstream.yml", mode: "100644", type: "blob", content: $workflow},
    {path: ".github/scripts/sync-cleaned-fork.sh", mode: "100755", type: "blob", content: $script}
  ])}' \
  | gh api "repos/${GITHUB_REPOSITORY}/git/trees" --input - --jq .sha)"

origin_sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/${CLEAN_BRANCH}" --jq .object.sha)"
origin_commit="$(gh api "repos/${GITHUB_REPOSITORY}/git/commits/${origin_sha}")"
origin_tree="$(jq -r .tree.sha <<< "${origin_commit}")"
origin_parent="$(jq -r '.parents[0].sha // ""' <<< "${origin_commit}")"

if [ "${origin_tree}" = "${new_tree}" ] && [ "${origin_parent}" = "${upstream_sha}" ]; then
  echo "Cleaned fork is already rebased on ${UPSTREAM_REPOSITORY}/${upstream_default}@${upstream_sha}."
  exit 0
fi

commit_sha="$(jq -n \
  --arg message "chore(sync): 🔧 sync cleaned fork with upstream" \
  --arg tree "${new_tree}" \
  --arg parent "${upstream_sha}" \
  '{message: $message, tree: $tree, parents: [$parent]}' \
  | gh api "repos/${GITHUB_REPOSITORY}/git/commits" --input - --jq .sha)"

latest_origin_sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/heads/${CLEAN_BRANCH}" --jq .object.sha)"
if [ "${latest_origin_sha}" != "${origin_sha}" ]; then
  echo "Ref refs/heads/${CLEAN_BRANCH} moved from ${origin_sha} to ${latest_origin_sha}; refusing to overwrite." >&2
  exit 1
fi

jq -n --arg sha "${commit_sha}" '{sha: $sha, force: true}' \
  | gh api -X PATCH "repos/${GITHUB_REPOSITORY}/git/refs/heads/${CLEAN_BRANCH}" --input - --jq .object.sha
