# 单阶段:app.jar 由构建脚本(javac + jar,存在数据库 app.script 里)先生成,这里只拷进 JRE 镜像。
FROM mirror.gcr.io/library/eclipse-temurin:21-jre
ARG VERSION=dev
ARG PROFILE=default
COPY app.jar /app/app.jar
RUN printf '%s' "$VERSION" > /app/version.txt && printf '%s' "$PROFILE" > /app/profile.txt
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
