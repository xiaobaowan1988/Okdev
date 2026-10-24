// deployer:原型。MySQL 存"哪个服务部署到哪个环境、副本数多少",对账循环把它翻译成 Argo CD Application。
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type config struct {
	listen, appNamespace, project, destServer, apiToken string
	reconcileEvery                                      time.Duration
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func kubeConfig() (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	return clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG")) // 本地调试用
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config{
		listen:         getenv("LISTEN", ":8080"),
		appNamespace:   getenv("APP_NAMESPACE", "argocd-apps-dev"),
		project:        getenv("ARGOCD_PROJECT", "project-dev"),
		destServer:     getenv("DEST_SERVER", "https://kubernetes.default.svc"),
		apiToken:       os.Getenv("API_TOKEN"),
		reconcileEvery: 10 * time.Second,
	}

	my := mysql.NewConfig()
	my.User = getenv("MYSQL_USER", "deployer")
	my.Passwd = os.Getenv("MYSQL_PASSWORD")
	my.Net, my.Addr = "tcp", getenv("MYSQL_ADDR", "mysql.deployer.svc:3306")
	my.DBName = getenv("MYSQL_DATABASE", "deployer")
	my.ParseTime, my.Loc = true, time.UTC
	db, err := sql.Open("mysql", my.FormatDSN())
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := &Store{db: db}
	for i := 0; ; i++ { // MySQL 可能还在启动
		if err = store.Migrate(ctx); err == nil {
			break
		}
		if i >= 30 {
			log.Error("migrate", "err", err)
			os.Exit(1)
		}
		log.Warn("waiting for mysql", "err", err)
		time.Sleep(2 * time.Second)
	}

	kc, err := kubeConfig()
	if err != nil {
		log.Error("kube config", "err", err)
		os.Exit(1)
	}
	kube, err := dynamic.NewForConfig(kc)
	if err != nil {
		log.Error("kube client", "err", err)
		os.Exit(1)
	}

	rec := NewReconciler(store, db, kube, cfg, log)
	go rec.Run(ctx)

	srv := &http.Server{Addr: cfg.listen, Handler: (&API{store: store, rec: rec, cfg: cfg, log: log}).routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.listen, "appNamespace", cfg.appNamespace)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
