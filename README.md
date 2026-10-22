# easyimage-go

一个不依赖 PHP 的自托管图床，可以直接替换 [EasyImage 2.0](https://github.com/icret/EasyImages2.0)：沿用原来的图片目录，**已经贴出去的图片链接一个都不会断**，PicGo 等上传工具也不用改配置。

- 单个可执行文件，Docker 镜像不到 20 MB，不需要数据库、PHP 或任何运行环境
- 网页上传：点击、拖拽、粘贴，一键复制链接 / Markdown / HTML / BBCode
- 后台按日期浏览、删除图片，删除的图片进回收目录，误删可以找回
- 兼容 EasyImage 的上传 API（`/api/index.php`，字段 `image` + `token`）和返回格式

> 本项目是社区独立实现的替代品，与 EasyImage 及其作者 [icret](https://github.com/icret) 没有任何关系，也不是官方的 Go 版本。名字里的 EasyImage 只用来说明它兼容的对象，代码没有复用 EasyImage 的任何部分。

## 为什么要换

2026 年 10 月公开的 PHP 核心漏洞 PHP2Shell（CVE-2026-109618 / CVE-2026-109619）可以在未登录的情况下远程执行代码。CISA 的通告说，所有处理网页请求的 PHP 都受影响，而且已经有人在实际利用（[CISA 通告](https://www.cisa.gov/news-events/alerts/2026/10/21/critical-vulnerabilities-php-require-immediate-patching)）。

EasyImage 2.8.6 及以下版本自身也有任意文件上传（[CVE-2025-65471](https://osv.dev/vulnerability/CVE-2025-65471)）和通过文件重命名执行代码（[CVE-2025-65474](https://nvd.nist.gov/vuln/detail/CVE-2025-65474)）等漏洞。截至 2026 年 10 月，没有查到官方的修复版本。

图床本质上只需要"收文件、存文件、发文件"。easyimage-go 把这三件事用 Go 重写，彻底去掉 PHP，读图片则完全交给 Nginx / OpenResty 当静态文件返回。

## 快速开始（Docker，全新安装）

```bash
mkdir easyimage-go && cd easyimage-go
curl -fsSLO https://raw.githubusercontent.com/Lau0x/easyimage-go/main/docker-compose.yml
curl -fsSL https://raw.githubusercontent.com/Lau0x/easyimage-go/main/.env.example -o .env
mkdir -p data && sudo chown 1000:1000 data
```

生成签名密钥，再生成一个 API token（给 PicGo 等工具用）：

```bash
sed -i "s/^EASYIMAGE_SECRET=.*/EASYIMAGE_SECRET=$(openssl rand -hex 32)/" .env
sed -i "s/^EASYIMAGE_TOKENS=.*/EASYIMAGE_TOKENS=$(openssl rand -hex 16)/" .env
```

然后编辑 `.env`，把 `EASYIMAGE_BASE` 改成你的图床网址，`EASYIMAGE_PASSWORD` 填一个足够长的后台密码。

```bash
chmod 600 .env
docker compose up -d
docker compose ps        # STATUS 显示 healthy 就是正常
```

容器只监听本机的 `127.0.0.1:18321`，还需要用 Nginx / OpenResty / Caddy 反向代理并配置 HTTPS，见下面的[反向代理](#反向代理)。

## 从 EasyImage 迁移

整个迁移不复制、不改动任何图片文件。新程序直接读写原来的 `i` 目录，所以旧链接自然有效；中途出问题，换回原来的站点配置就能回滚。

下面用 `<站点根目录>` 指 EasyImage 所在目录，也就是里面有 `i`、`config`、`api` 等文件夹的那个目录。常见位置：

| 环境 | 宿主机上的站点根目录 |
| --- | --- |
| 1Panel | `/opt/1panel/apps/openresty/openresty/www/sites/<域名>/index` |
| 宝塔 | `/www/wwwroot/<域名>` |
| 自己装的 Nginx | 看站点配置里的 `root` |

### 1. 备份

```bash
tar czf ~/easyimage-backup-$(date +%F).tgz -C <站点根目录> .
```

### 2. 检查有没有被植入后门

EasyImage 的漏洞允许上传可执行文件。迁移前先看看图片目录里有没有不是图片的东西：

```bash
find <站点根目录>/i -type f ! -iregex '.*\.\(jpe?g\|png\|gif\|webp\|bmp\|ico\|avif\|svg\)'
```

如果有输出（尤其是 `.php`、`.phtml`、`.phar`），说明服务器可能已经被入侵。这时不要只换程序，要把服务器当作已失陷处理：排查其他站点、改掉所有密码和密钥，必要时重装系统。

easyimage-go 部署后不会执行、也不会输出这些文件，但在宿主机上它们仍然存在。

### 3. 导出原来的 API Token

如果你在用 PicGo、ShareX 等工具通过 API 上传，把原来的 token 导出来，后面填进 `EASYIMAGE_TOKENS`，工具那边就什么都不用改：

```bash
grep -oE "'[0-9a-f]{32}'" <站点根目录>/config/api_key.php | tr -d "'" | paste -sd,
```

这条命令会导出所有 token，包括已经过期的。easyimage-go 不区分过期时间，只保留你还在用的就行。

### 4. 查看图片目录的属主

```bash
stat -c '%u:%g' <站点根目录>/i
```

记下输出的两个数字，比如 `1000:1000`，下一步要用。新程序必须用同一个用户身份运行，才能往原来的目录里写新图片。

### 5. 启动 easyimage-go

**方式一：Docker（推荐）**

```bash
mkdir ~/easyimage-go && cd ~/easyimage-go
curl -fsSLO https://raw.githubusercontent.com/Lau0x/easyimage-go/main/docker-compose.yml
curl -fsSL https://raw.githubusercontent.com/Lau0x/easyimage-go/main/.env.example -o .env
```

编辑 `docker-compose.yml`，把 `volumes` 改成挂载原来的 `i` 目录：

```yaml
    volumes:
      - <站点根目录>/i:/data/i
      - ./i-trash:/data/i-trash
```

编辑 `.env`：`EASYIMAGE_BASE` 填原来的图床网址（比如 `https://img.example.com`），`EASYIMAGE_TOKENS` 填第 3 步导出的 token，`EASYIMAGE_UID` / `EASYIMAGE_GID` 填第 4 步看到的数字，再填好密码和密钥。然后：

```bash
mkdir -p i-trash && sudo chown <第4步的数字> i-trash
chmod 600 .env
docker compose up -d
curl -s http://127.0.0.1:18321/healthz     # 输出 ok 就是正常
```

**方式二：直接运行（systemd）**

从 [Releases](https://github.com/Lau0x/easyimage-go/releases) 下载对应架构的程序（`uname -m` 显示 `x86_64` 选 amd64，`aarch64` 选 arm64）：

```bash
sudo install -m 755 easyimage-go-linux-amd64 /usr/local/bin/easyimage-go
sudo cp .env.example /etc/easyimage-go.env && sudo chmod 600 /etc/easyimage-go.env   # 然后编辑填好
sudo mkdir -p <站点根目录>/i-trash && sudo chown <第4步的数字> <站点根目录>/i-trash
```

把 [deploy/easyimage-go.service](deploy/easyimage-go.service) 复制到 `/etc/systemd/system/`，把里面的路径改成你的 `<站点根目录>`，`User` / `Group` 改成图片目录的属主。然后：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now easyimage-go
curl -s http://127.0.0.1:18321/healthz
```

### 6. 改反向代理，下线 PHP

按下面的[反向代理](#反向代理)替换站点配置。**一定要删掉原来所有 PHP 相关的配置**，包括 `location ~ \.php$`、`fastcgi_pass`、`include enable-php-*.conf` 和 `index index.php`。只要还留着一条，PHP 请求就会被交给 PHP 处理，绕过 easyimage-go。

```bash
nginx -t && nginx -s reload          # 1Panel 在面板里保存配置会自动重载
```

### 7. 验证

```bash
# 旧图片：应返回 200 和 image/...
curl -sI https://img.example.com/i/<一张旧图的路径> | head -3
# 上传目录里的脚本：应返回 404
curl -s -o /dev/null -w '%{http_code}\n' https://img.example.com/i/test.php
# 旧 API：应返回 "result":"success"
curl -s -F image=@test.png -F token=<你的token> https://img.example.com/api/index.php
```

再打开 `https://img.example.com` 用新密码登录，试一下网页上传和后台管理。

### 8. 清理

确认一切正常后：

- 停掉这个站点的 PHP（1Panel 里删除或停用对应的 PHP 运行环境；宝塔里把站点的 PHP 版本改成"纯静态"）
- 把 EasyImage 的程序文件移出站点目录，只保留 `i` 目录。备份已经在第 1 步做过了
- 服务器上如果还有其他 PHP 站点，把 PHP 升级到修复版本（8.5.12 / 8.4.27 / 8.3.36 / 8.2.35 或更高）

### 迁移后的差异

| 项目 | 情况 |
| --- | --- |
| `/i/...` 图片链接 | 不变 |
| 开启"隐藏 /i/ 路径"后的 `/年/月/日/文件名` 链接 | 不变 |
| `thumb.php?img=...` 缩略图链接 | 跳转到原图 |
| `/api/index.php` 上传 | 不变，返回格式一致 |
| 原来的删除链接 `del.php?hash=...` | **失效**，新上传的图片会返回新的删除链接 |
| 原图保护 `hide.php`、水印、压缩、格式转换、鉴黄 | 不支持 |
| 多用户、按 token 分目录、token 过期时间 | 不支持，所有 token 权限相同、长期有效 |
| 上传格式 | 新上传只接受 jpg / png / gif / webp；旧的 bmp / ico / svg 等仍可访问 |

## 反向代理

完整配置见 [deploy/nginx.conf](deploy/nginx.conf)，Nginx 和 OpenResty 通用。核心是三段：

```nginx
client_max_body_size 12m;

# 图片由 Nginx 直接返回，不经过任何程序；图片以外的文件一律 404
location ^~ /i/ {
    root <OpenResty 看到的站点根目录>;
    location ~* \.(jpe?g|png|gif|webp|bmp|ico|avif)$ { expires 30d; try_files $uri =404; }
    return 404;
}

# 旧的 .php 地址（api/index.php、thumb.php）交给 easyimage-go 处理
location ~* \.php$ { proxy_pass http://127.0.0.1:18321; proxy_set_header Host $host; proxy_set_header X-Real-IP $remote_addr; }

location / { proxy_pass http://127.0.0.1:18321; proxy_set_header Host $host; proxy_set_header X-Real-IP $remote_addr; }
```

几点说明：

- **1Panel**：OpenResty 跑在容器里，`root` 要写容器内的路径 `/www/sites/<域名>/index`，不是宿主机的 `/opt/1panel/...`。1Panel 的 OpenResty 默认使用 host 网络，`127.0.0.1:18321` 可以直接访问。
- **全新安装**：如果图片不在 Nginx 能读到的目录里，可以省掉 `/i/` 那一段，全部反代给 easyimage-go，由它自己返回图片。这样更简单，只是性能稍差。
- **`Host` 和 `X-Real-IP` 两行不能少**：前者用于防跨站提交的来源校验，后者用于登录限流按真实 IP 计数。
- **Caddy**：`reverse_proxy 127.0.0.1:18321` 即可，Caddy 默认会传递 Host。需要额外加 `header_up X-Real-IP {remote_host}`。

## 上传工具配置

**PicGo**：安装"自定义 Web 图床"插件（web-uploader），然后这样填：

| 项目 | 值 |
| --- | --- |
| API 地址 | `https://img.example.com/api/index.php` |
| POST 参数名 | `image` |
| JSON 路径 | `url` |
| 自定义 Body | `{"token":"你的token"}` |

原来已经配置过 EasyImage 的，不需要改。

**curl / 脚本**：

```bash
curl -F image=@photo.jpg -F token=你的token https://img.example.com/api/index.php
# 也可以用请求头传 token
curl -H "Authorization: Bearer 你的token" -F image=@photo.jpg https://img.example.com/api/upload
```

返回：

```json
{"result":"success","code":200,"url":"https://img.example.com/i/2026/10/22/k88e7pabcd.jpg","srcName":"photo","thumb":"...","del":"https://img.example.com/del?f=...&s=...","message":"success"}
```

失败时 `result` 为 `failed`，`code` 含义：`202` token 错误，`204` 没有文件，`400` 格式不支持或文件过大，`500` 服务器保存失败。

## 配置项

所有配置都可以用环境变量设置；直接运行时也可以用命令行参数，参数优先于环境变量。

| 环境变量 | 命令行参数 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `EASYIMAGE_BASE` | `-base` | 无，必填 | 图床对外网址，返回的链接以它开头 |
| `EASYIMAGE_PASSWORD` | 无 | 无，必填 | 后台密码，至少 8 位 |
| `EASYIMAGE_SECRET` | 无 | 无，必填 | 签名密钥，至少 32 个字符。更换后所有人需要重新登录，旧的删除链接失效 |
| `EASYIMAGE_TOKENS` | 无 | 空 | API token，逗号分隔。留空则关闭 API 上传 |
| `EASYIMAGE_IMAGES` | `-images` | Docker 内为 `/data/i` | 图片目录，对应网址里的 `/i/` |
| `EASYIMAGE_TRASH` | `-trash` | 图片目录旁的 `i-trash` | 删除的图片移到这里 |
| `EASYIMAGE_MAX_MB` | `-max-mb` | `10` | 单张图片大小上限（MB）。调大时 Nginx 的 `client_max_body_size` 也要跟着改 |
| `EASYIMAGE_LISTEN` | `-listen` | `127.0.0.1:18321`，Docker 内为 `:18321` | 监听地址 |
| `EASYIMAGE_TRUSTED_PROXIES` | `-trusted-proxies` | `127.0.0.0/8,::1/128`，Docker 内另加私有网段 | 只有来自这些地址的请求，才会采信 `X-Real-IP` |

## 安全设计

- **不执行任何上传的内容**：没有 PHP，图片以静态文件返回，并带 `X-Content-Type-Options: nosniff`；SVG 额外加了 `sandbox` 的 CSP，里面的脚本不会执行
- **按文件内容判断格式**：不信任文件名和 `Content-Type`，读取文件头并解码图片尺寸，伪装成图片的脚本会被拒绝；文件名由服务端随机生成
- **路径校验**：拒绝 `..`、绝对路径、反斜杠和以点开头的路径段；只输出图片扩展名的文件，不列目录
- **登录**：登录状态用 HMAC 签名的 Cookie（HttpOnly、SameSite=Lax、HTTPS 下带 Secure）；同一 IP 15 分钟内连续输错 5 次锁定
- **防跨站提交**：带 Cookie 的写操作都会校验 `Origin` / `Referer`
- **删除**：API 返回的删除链接有签名，打开后只显示确认页，点确认才删；删除是移到回收目录
- **最小权限**：Docker 镜像基于 distroless，没有 shell，以非 root 运行；systemd 配置启用了 `ProtectSystem=strict` 等沙箱选项，只开放图片目录的写权限

发现安全问题请通过 GitHub 的 [Security Advisories](https://github.com/Lau0x/easyimage-go/security/advisories/new) 私下报告，不要直接开 issue。

## 从源码构建

需要 Go 1.18 或以上版本，只用标准库，没有第三方依赖。

```bash
git clone https://github.com/Lau0x/easyimage-go.git && cd easyimage-go
CGO_ENABLED=0 go build -o easyimage-go .
bash test/e2e.sh          # 端到端测试：模拟 EasyImage 站点，检查兼容性和安全项
docker build -t easyimage-go .  # 构建镜像
```

## License

[MIT](LICENSE)
