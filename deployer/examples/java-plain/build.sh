#!/bin/sh
# 构建脚本:在 JDK 镜像里运行,工作目录是 context_path(src/java-plain)。没有 Maven,javac + jar 打 jar。
set -eu
mkdir -p out
javac -d out src/*.java
printf 'Main-Class: Main\n' > manifest.txt
jar cfm app.jar manifest.txt -C out .
ls -l app.jar
