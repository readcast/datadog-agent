# Unless explicitly stated otherwise all files in this repository are licensed
# under the Apache License Version 2.0.
# This product includes software developed at Datadog (https:#www.datadoghq.com/).
# Copyright 2016-2020 Datadog, Inc.

name 'libclang'

build do
  if ENV.has_key?('LIBCLANG_TARBALL') and not ENV['LIBCLANG_TARBALL'].empty?
    # tarball contains only the lib/clang/ directory for use as the -resource-dir during ebpf compilation
    command "tar -xvf #{ENV['LIBCLANG_TARBALL']} -C #{install_dir}/embedded"
  end
end
