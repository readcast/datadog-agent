from __future__ import print_function


from invoke import task

from .deploy.trigger_agent_pipeline import trigger_agent_pipeline, wait_for_pipeline
from .deploy.gitlab import Gitlab


@task
def trigger(
    ctx,
    ref="master",
    release_version_6="nightly",
    release_version_7="nightly-a7",
    repo_branch="nightly",
    windows_update_latest=True,
):
    pipeline_id = trigger_agent_pipeline(ref, release_version_6, release_version_7, repo_branch, windows_update_latest)
    wait_for_pipeline(pipeline_id)


@task
def follow(ctx, id=None, ref=None, here=False):
    if id:
        wait_for_pipeline(id)
    elif ref:
        wait_for_pipeline(Gitlab().pipelines_for_ref("DataDog/datadog-agent", ref)[0]['id'])
    elif here:
        ref = ctx.run("git rev-parse --abbrev-ref HEAD", hide=True).stdout.strip()
        pipelines = Gitlab().pipelines_for_ref("DataDog/datadog-agent", ref)
        if len(pipelines) > 0:
            wait_for_pipeline(pipelines[0]['id'])
        else:
            print("No pipelines found for {ref}".format(ref=ref))
