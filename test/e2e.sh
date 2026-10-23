#!/usr/bin/env bash
# 端到端测试：编译程序，在临时目录里模拟一个 EasyImage 站点，逐项检查兼容性和安全性
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"
PORT="${PORT:-18399}"
B="http://127.0.0.1:$PORT"
O="Origin: $B"
J="$W/cookies.txt"
TOKEN=1c17b11693cb5ec63859b091c5b9c1b2
FAILS=0

(cd "$ROOT" && CGO_ENABLED=0 go build -tags nodynamic -o "$W/picnook" . \
  && CGO_ENABLED=0 go build -tags nodynamic -o "$W/fixtool" ./test/fixtures) || exit 1
"$W/fixtool" "$W" || exit 1

start_server() {
  PICNOOK_PASSWORD=testpass123 PICNOOK_SECRET=0123456789abcdef0123456789abcdef PICNOOK_TOKENS=$TOKEN \
  PICNOOK_LISTEN=127.0.0.1:$PORT PICNOOK_IMAGES="$W/site/i" PICNOOK_BASE="$B" PICNOOK_EXTRA_DIRS="images=$W/site/images" \
    "$W/picnook" >> "$W/server.log" 2>&1 &
  PID=$!
  for _ in $(seq 50); do curl -s "$B/healthz" >/dev/null && break; sleep 0.1; done
}
start_server
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -rf "$W"' EXIT

code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
field() { grep -o "\"$1\":\"[^\"]*\"" | cut -d'"' -f4; }
check() {
  if [ "$2" = "$3" ]; then echo "PASS  $1"; else echo "FAIL  $1（期望 $3，实际 $2）"; FAILS=$((FAILS + 1)); fi
}

echo "== 旧链接兼容 =="
check "旧图片 /i/2024/05/03/old123.jpg" "$(code $B/i/2024/05/03/old123.jpg)" 200
check "旧图片 Content-Type" "$(curl -sI $B/i/2024/05/03/old123.jpg | grep -i '^content-type' | tr -d '\r' | awk '{print $2}')" image/jpeg
check "旧缩略图 /i/cache/" "$(code $B/i/cache/2024_05_03_old123.png)" 200
check "hide_path 链接 /2024/05/03/old123.jpg" "$(code $B/2024/05/03/old123.jpg)" 200
check "thumb.php 跳转到原图" "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$B/app/thumb.php?img=/i/2024/05/03/old123.jpg")" "301 $B/i/2024/05/03/old123.jpg"
check "图片带一个跨域头" "$(curl -sI -H 'Origin: https://example.org' $B/i/2024/05/03/old123.jpg | grep -ci '^access-control-allow-origin: \*')" 1
check "Range 请求" "$(code -H 'Range: bytes=0-9' $B/i/2024/05/03/old123.jpg)" 206
check "旧目录 /images/ 的图片" "$(code $B/images/2021/04/06/legacy.th.jpg)" 200
check "旧目录里的 .php 不输出" "$(code $B/images/2021/04/06/evil.php)" 404
check "旧目录编码穿越" "$(code -L --path-as-is "$B/images/2021/..%2f..%2f..%2fi/2024/05/03/shell.php")" 404
check "旧目录不列出" "$(code $B/images/2021/04/06/)" 404

echo "== 安全 =="
check "残留 shell.php 不输出" "$(code $B/i/2024/05/03/shell.php)" 404
check "路径穿越 /i/../" "$(code -L --path-as-is $B/i/../../picnook)" 404
check "编码穿越 %2e%2e" "$(code -L --path-as-is "$B/i/%2e%2e/%2e%2e/picnook")" 404
check "编码穿越 ..%2f" "$(code -L --path-as-is "$B/i/2024/..%2f..%2f..%2fpicnook")" 404
check "目录不列出" "$(code $B/i/2024/05/03/)" 404
check "thumb.php 不能跳到外站" "$(code "$B/app/thumb.php?img=//evil.com/a.jpg")" 404
check "未登录访问首页跳登录" "$(code $B/)" 303
check "未登录访问管理页" "$(code $B/admin)" 303
check "未登录网页上传" "$(curl -s -H "$O" -F image=@$W/fixtures/a.png $B/upload | grep -o '"code":[0-9]*')" '"code":401'

echo "== 登录 =="
check "无 Origin 登录被拒" "$(code -d password=testpass123 $B/login)" 403
check "错误密码" "$(code -H "$O" -d password=wrong $B/login)" 401
check "正确密码" "$(code -c $J -H "$O" -d password=testpass123 $B/login)" 303
check "登录后首页" "$(code -b $J $B/)" 200
check "伪造 Cookie" "$(code -b 'picnook_session=9999999999.deadbeef' $B/)" 303

echo "== 网页上传 =="
R=$(curl -s -b $J -H "$O" -F image=@$W/fixtures/a.png $B/upload)
check "上传 PNG" "$(echo "$R" | grep -o '"code":[0-9]*')" '"code":200'
URL=$(echo "$R" | field url)
check "新图片可访问" "$(code "$URL")" 200
check "跨站上传被拒" "$(curl -s -b $J -H 'Origin: https://evil.com' -F image=@$W/fixtures/a.png $B/upload | grep -o '"code":[0-9]*')" '"code":401'
check "伪装成 jpg 的 PHP" "$(curl -s -b $J -H "$O" -F image=@$W/fixtures/shell.jpg $B/upload | grep -o '"code":[0-9]*')" '"code":400'
check "JPEG 文件头 + PHP" "$(curl -s -b $J -H "$O" -F image=@$W/fixtures/fake.jpg $B/upload | grep -o '"code":[0-9]*')" '"code":400'
check "超过大小上限" "$(curl -s -b $J -H "$O" -F image=@$W/fixtures/big.png $B/upload | grep -o '"code":[0-9]*')" '"code":400'

echo "== EasyImage 兼容 API =="
R=$(curl -s -F image=@$W/fixtures/a.png -F token=$TOKEN $B/api/index.php)
check "API 上传" "$(echo "$R" | grep -o '"result":"success"')" '"result":"success"'
check "API 错误 Token" "$(curl -s -F image=@$W/fixtures/a.png -F token=bad $B/api/index.php | grep -o '"code":[0-9]*')" '"code":202'
check "API 没有文件" "$(curl -s -F token=$TOKEN $B/api/index.php | grep -o '"code":[0-9]*')" '"code":204'
check "API Bearer Token" "$(curl -s -H "Authorization: Bearer $TOKEN" -F image=@$W/fixtures/a.png $B/api/upload | grep -o '"code":[0-9]*')" '"code":200'
DEL=$(echo "$R" | field del)
AURL=$(echo "$R" | field url)
check "删除链接 GET 只显示确认页" "$(code "$DEL")" 200
check "GET 之后图片还在" "$(code "$AURL")" 200
check "篡改签名" "$(code "${DEL%?}0")" 403
F=$(echo "$DEL" | sed -E 's/.*[?&]f=([^&]*).*/\1/')
S=$(echo "$DEL" | sed -E 's/.*[?&]s=([^&]*).*/\1/')
curl -s -o /dev/null -H "$O" -d "f=$F&s=$S" $B/del
check "POST 确认后删除" "$(code "$AURL")" 404

echo "== 管理后台 =="
check "管理页" "$(code -b $J $B/admin)" 200
check "列出旧图" "$(curl -s -b $J "$B/admin?d=2024/05/03" | grep -c 'class="name">old123.jpg')" 1
check "不列出 shell.php" "$(curl -s -b $J "$B/admin?d=2024/05/03" | grep -c shell.php)" 0
check "后台删除拒绝穿越路径" "$(curl -s -o /dev/null -w '%{redirect_url}' -b $J -H "$O" -d 'f=../../picnook' $B/admin/delete | grep -o 'msg=[^&]*')" "msg=%E8%B7%AF%E5%BE%84%E6%97%A0%E6%95%88"
REL=${URL#$B/i/}
curl -s -o /dev/null -b $J -H "$O" -d "f=$REL" $B/admin/delete
check "后台删除新图" "$(code "$URL")" 404
check "删除的图在回收目录" "$(test -f "$W/site/i-trash/$REL" && echo yes)" yes
check "程序文件未受影响" "$(test -f "$W/picnook" && echo yes)" yes

echo "== 登录限流 =="
for _ in 1 2 3 4 5; do curl -s -o /dev/null -H "$O" -H 'X-Real-IP: 203.0.113.1' -d password=wrong $B/login; done
check "连续失败后锁定该 IP" "$(code -H "$O" -H 'X-Real-IP: 203.0.113.1' -d password=testpass123 $B/login)" 429
check "其他 IP 不受影响" "$(code -H "$O" -H 'X-Real-IP: 203.0.113.2' -d password=testpass123 $B/login)" 303

P="$W/fixtool"
probe() { curl -s -o "$W/dl" "$1"; "$P" probe "$W/dl"; }
save() { curl -s -o /dev/null -w '%{http_code}' -b $J -H "$O" --data "$1" $B/admin/settings; }
tokact() { curl -s -o /dev/null -b $J -H "$O" --data "$1" $B/admin/tokens; }
api() { curl -s -F image=@"$1" -F token="$2" $B/api/index.php; }

echo "== 后台设置 =="
check "未登录访问设置页" "$(code $B/admin/settings)" 303
check "设置页" "$(code -b $J $B/admin/settings)" 200
check "无 Origin 保存设置被拒" "$(code -b $J --data 'max_mb=10' $B/admin/settings)" 403
check "环境变量里的 token 已导入后台" "$(curl -s -b $J $B/admin/settings | grep -c "tokens/files?t=$TOKEN")" 1

echo "== Token 管理 =="
T2=abcdef0123456789abcdef0123456789
tokact "action=add&value=$T2&days=0&note=picgo"
check "新增 token 可以上传" "$(api $W/fixtures/a.png $T2 | grep -o '"code":[0-9]*')" '"code":200'
check "返回 token ID" "$(api $W/fixtures/a.png $T2 | grep -o '"id":[0-9]*')" '"id":2'
tokact "action=disable&value=$T2"
check "禁用后上传被拒" "$(api $W/fixtures/a.png $T2 | field message)" "Token 已禁用"
tokact "action=enable&value=$T2"
check "重新启用后可以上传" "$(api $W/fixtures/a.png $T2 | grep -o '"code":[0-9]*')" '"code":200'
check "文件页列出该 token 的 3 张图" "$(curl -s -b $J "$B/admin/tokens/files?t=$T2" | grep -c 'class="name"')" 3
check "设置页显示上传数量" "$(curl -s -b $J "$B/admin/settings?q=picgo" | grep -c '<td>3</td>')" 1
tokact "action=purge&value=$T2"
check "删除上传后文件页为空" "$(curl -s -b $J "$B/admin/tokens/files?t=$T2" | grep -c 'class="name"')" 0
tokact "action=delete&value=$T2"
check "删除 token 后上传被拒" "$(api $W/fixtures/a.png $T2 | field message)" "Token 错误"
check "无 Origin 管理 token 被拒" "$(code -b $J --data "action=delete&value=$TOKEN" $B/admin/tokens)" 403
PHP="<?php \$tokenList=Array( 'aaaabbbbccccddddeeeeffff00001111'=>Array( 'id'=>5, 'expired'=>4102444800, 'add_time'=>1680497928 ), '99998888777766665555444433332222'=>Array( 'id'=>6, 'expired'=>1680584583, 'add_time'=>1680498183 ) );"
curl -s -o /dev/null -b $J -H "$O" --data-urlencode "php=$PHP" $B/admin/tokens/import
check "导入的 EasyImage token 保留原 ID" "$(api $W/fixtures/a.png aaaabbbbccccddddeeeeffff00001111 | grep -o '"id":[0-9]*')" '"id":5'
check "导入的过期 token 被拒" "$(api $W/fixtures/a.png 99998888777766665555444433332222 | field message)" "Token 已过期"

echo "== 图片处理 =="
BASESET="max_mb=10&quality=80&water_pos=9&water_size=6&water_color=%23000000&water_opacity=80&allow_empty_referer=on"
save "$BASESET&compress=on" >/dev/null
ORIG=$(wc -c < $W/fixtures/photo.jpg)
U=$(api $W/fixtures/photo.jpg $TOKEN | field url)
NEW=$(curl -s "$U" | wc -c)
echo "      压缩前 $ORIG 字节，压缩后 $NEW 字节"
check "压缩后变小" "$([ "$NEW" -lt "$ORIG" ] && echo yes)" yes
check "压缩后尺寸和格式不变" "$(probe "$U" | cut -d' ' -f1,2)" "800x600 jpeg"
U=$(api $W/fixtures/rot6.jpg $TOKEN | field url)
check "按 EXIF 方向转正" "$(probe "$U" | cut -d' ' -f1)" "80x160"
save "$BASESET&convert=webp" >/dev/null
U=$(api $W/fixtures/photo.jpg $TOKEN | field url)
check "转 WebP 后链接扩展名" "${U##*.}" webp
check "转 WebP 后内容" "$(probe "$U" | cut -d' ' -f1,2)" "800x600 webp"
save "$BASESET&watermark=on&water_text=TEST" >/dev/null
U=$(api $W/fixtures/white.png $TOKEN | field url)
check "白图加上了水印" "$([ "$(probe "$U" | cut -d' ' -f3)" -gt 0 ] && echo yes)" yes
U=$(api $W/fixtures/a.png $TOKEN | field url)
check "小于 100 像素的图不加水印" "$(probe "$U")" "8x8 png 1"
check "水印预览" "$(curl -s -o /dev/null -w '%{http_code} %{content_type}' -b $J $B/admin/settings/preview)" "200 image/png"
save "$BASESET&watermark=on&water_text=%E4%B8%AD%E6%96%87%E6%B0%B4%E5%8D%B0" >/dev/null
check "内置字体包含常用汉字" "$(curl -s -b $J $B/admin/settings | grep -c '当前字体不包含')" 0
U=$(api $W/fixtures/white.png $TOKEN | field url)
check "中文水印画到了图上" "$([ "$(probe "$U" | cut -d' ' -f3)" -gt 0 ] && echo yes)" yes
save "$BASESET&watermark=on&water_text=%F0%AA%9A%A5" >/dev/null
check "生僻字提示缺字" "$(curl -s -b $J $B/admin/settings | grep -c '当前字体不包含')" 1
check "上传无效字体被拒" "$(curl -s -o /dev/null -w '%{redirect_url}' -b $J -H "$O" -F font=@$W/fixtures/font.ttf $B/admin/font | grep -c '%E5%AD%97%E4%BD%93%E6%97%A0%E6%B3%95%E8%AF%86%E5%88%AB')" 1
save "$BASESET" >/dev/null
U=$(api $W/fixtures/photo.jpg $TOKEN | field url)
check "关闭处理后原样保存" "$(curl -s "$U" | cmp -s - $W/fixtures/photo.jpg && echo same)" same

echo "== 防盗链 =="
IMG=$B/i/2024/05/03/old123.jpg
save "$BASESET&hotlink=on&allowed_referers=blog.example.org%0A*.friend.net" >/dev/null
check "外站引用被拒" "$(code -H 'Referer: https://evil.com/p' $IMG)" 403
check "白名单放行" "$(code -H 'Referer: https://blog.example.org/post/1' $IMG)" 200
check "通配子域名放行" "$(code -H 'Referer: https://a.friend.net/' $IMG)" 200
check "通配不放行相似域名" "$(code -H 'Referer: https://evilfriend.net/' $IMG)" 403
check "本站放行" "$(code -H "Referer: $B/admin" $IMG)" 200
check "空 Referer 放行" "$(code $IMG)" 200
check "带 Vary: Referer" "$(curl -sI $IMG | grep -ci '^vary: referer')" 1
save "max_mb=10&quality=80&hotlink=on" >/dev/null
check "关闭直接访问后空 Referer 被拒" "$(code $IMG)" 403
save "$BASESET" >/dev/null
check "关闭防盗链后外站可用" "$(code -H 'Referer: https://evil.com/p' $IMG)" 200

echo "== 批量删除 =="
U1=$(api $W/fixtures/a.png $TOKEN | field url)
U2=$(api $W/fixtures/white.png $TOKEN | field url)
check "批量删除提示删除了 2 张" "$(curl -s -o /dev/null -w '%{redirect_url}' -b $J -H "$O" --data "f=${U1#$B/i/}&f=${U2#$B/i/}&back=/admin" $B/admin/delete | grep -c 'msg=%E5%B7%B2%E5%88%A0%E9%99%A4+2')" 1
check "两张图都已删除" "$(code "$U1") $(code "$U2")" "404 404"
check "删除后回到指定页面" "$(curl -s -o /dev/null -w '%{redirect_url}' -b $J -H "$O" --data "f=nope.png&back=/admin/settings" $B/admin/delete | cut -d'?' -f1)" "$B/admin/settings"
check "回跳地址不能指向外站" "$(curl -s -o /dev/null -w '%{redirect_url}' -b $J -H "$O" --data "f=nope.png&back=//evil.com/admin" $B/admin/delete | grep -c evil.com)" 0
check "日期条显示每天的数量" "$([ "$(curl -s -b $J $B/admin | grep -c '张</small>')" -gt 0 ] && echo yes)" yes

echo "== 修改密码 =="
J2="$W/cookies2.txt"
curl -s -o /dev/null -c $J2 -H "$O" -d password=testpass123 $B/login
pw() { curl -s -o /dev/null -w '%{redirect_url}' -b $J -H "$O" --data "$1" $B/admin/password; }
check "无 Origin 改密码被拒" "$(code -b $J --data 'current=testpass123&new=newpass12345&confirm=newpass12345' $B/admin/password)" 403
check "当前密码错误被拒" "$(pw 'current=wrong&new=newpass12345&confirm=newpass12345' | grep -c 'msg=%E5%BD%93%E5%89%8D')" 1
check "两次输入不一致被拒" "$(pw 'current=testpass123&new=newpass12345&confirm=other12345' | grep -c 'msg=%E4%B8%A4')" 1
check "新密码太短被拒" "$(pw 'current=testpass123&new=short&confirm=short' | grep -c 'msg=%E6%96%B0')" 1
check "被拒后密码没变" "$(code -H "$O" -d password=testpass123 $B/login)" 303
check "修改密码成功" "$(curl -s -o /dev/null -w '%{http_code}' -b $J -c $J -H "$O" --data 'current=testpass123&new=newpass12345&confirm=newpass12345' $B/admin/password)" 303
check "当前设备保持登录" "$(code -b $J $B/admin)" 200
check "其他设备的登录失效" "$(code -b $J2 $B/admin)" 303
check "旧密码不能登录" "$(code -H "$O" -d password=testpass123 $B/login)" 401
check "新密码可以登录" "$(code -H "$O" -d password=newpass12345 $B/login)" 303
check "配置里不存明文密码" "$(grep -c 'newpass12345' $W/site/picnook-config/settings.json)" 0
check "配置里存的是加盐哈希" "$(grep -c '"password_hash": "pbkdf2-sha256\$600000\$' $W/site/picnook-config/settings.json)" 1
check "设置页显示正在用后台密码" "$(curl -s -b $J $B/admin/settings | grep -c '当前使用后台设置的密码')" 1

echo "== 重置密码 =="
check "reset-password 清除后台密码" "$("$W/picnook" reset-password "$W/site/picnook-config" >/dev/null 2>&1; echo $?) $(grep -c password_hash $W/site/picnook-config/settings.json)" "0 0"
kill $PID; wait $PID 2>/dev/null; start_server
check "重启后用 .env 密码登录" "$(code -H "$O" -d password=testpass123 $B/login)" 303
check "重置后 token 等设置保留" "$(api $W/fixtures/a.png $TOKEN | grep -o '"code":[0-9]*')" '"code":200'
check "再次重置时提示无需重置" "$("$W/picnook" reset-password "$W/site/picnook-config" 2>&1 | grep -c '无需重置')" 1

echo "== 健康检查 =="
check "healthcheck 子命令" "$(PICNOOK_LISTEN=127.0.0.1:$PORT "$W/picnook" healthcheck; echo $?)" 0

echo
if [ "$FAILS" -gt 0 ]; then
  echo "$FAILS 项失败，服务日志："
  cat "$W/server.log"
  exit 1
fi
echo "全部通过"
