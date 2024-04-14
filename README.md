# Apple Music ALAC Downloader
Original script by Sorrow. Modified by me to include some fixes and improvements.

## How to use
1. Create a virtual device on Android Studio with a image that doesn't have Google APIs.
2. Install this version of Apple Music: https://www.apkmirror.com/apk/apple/apple-music/apple-music-3-6-0-beta-release/apple-music-3-6-0-beta-4-android-apk-download/. You will also need SAI to install it: https://f-droid.org/pt_BR/packages/com.aefyr.sai.fdroid/.
3. Launch Apple Music and sign in to your account. Subscription required.
4. Port forward 10020 TCP: `adb forward tcp:10020 tcp:10020`.
5. Start frida server.
6. Start the frida agent: `frida -U -l agent.js -f com.apple.android.music`.
7. Start downloading some albums: `go run main.go https://music.apple.com/us/album/whenever-you-need-somebody-2022-remaster/1624945511`.

## 使用

我简单修改了一下原脚本，加了gin web服务，方便使用，传入专辑链接即可下载。

配置好frida和adb后修改配置文件
`frida_path:  frida #电脑上的frida路径
frida_server_path : /data/local/tmp/fs #手机上的frida server路径
port: 8080 #web服务端口`

get方式请求就行了
http://127.0.0.1:8080/applemusic/addDownload?url=https://music.apple.com/cn/album/1989-taylors-version-deluxe/1713845538

下载目录默认是 ./AM-DL%20downloads  可以直接用alist共享文件夹，方便下载
