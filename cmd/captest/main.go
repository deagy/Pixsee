package main

import (
	"fmt"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xinerama"
	"github.com/jezek/xgb/xproto"
	"github.com/kbinani/screenshot"
)

func direct() {
	conn, err := xgb.NewConn()
	if err != nil {
		fmt.Println("direct NewConn err:", err)
		return
	}
	defer conn.Close()
	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root
	w := setup.DefaultScreen(conn).WidthInPixels
	h := setup.DefaultScreen(conn).HeightInPixels
	fmt.Printf("direct: root=%d screen=%dx%d\n", root, w, h)
	if err := xinerama.Init(conn); err != nil {
		fmt.Println("direct xinerama.Init err:", err)
		return
	}
	reply, err := xinerama.QueryScreens(conn).Reply()
	if err != nil {
		fmt.Println("direct QueryScreens err:", err)
		return
	}
	fmt.Printf("direct: Number=%d\n", reply.Number)
	for i, s := range reply.ScreenInfo {
		fmt.Printf("  direct screen[%d] XOrg=%d YOrg=%d W=%d H=%d\n", i, s.XOrg, s.YOrg, s.Width, s.Height)
	}
}

func lib(n int) {
	fmt.Printf("lib call #%d: NumActiveDisplays=%d\n", n, screenshot.NumActiveDisplays())
	for i := 0; i < n; i++ {
		b := screenshot.GetDisplayBounds(i)
		fmt.Printf("  lib display %d bounds=%+v\n", i, b)
	}
}

func main() {
	direct()
	lib(3)
}
