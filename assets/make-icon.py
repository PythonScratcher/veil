import sys
from PIL import Image, ImageDraw, ImageFilter
out=sys.argv[1]
S=1024
def master():
    # gradient squircle
    g=Image.new("RGBA",(S,S))
    top=(146,112,255); bot=(88,52,214)
    px=g.load()
    for y in range(S):
        for x in range(S):
            t=(x*0.35+y*0.65)/S
            px[x,y]=tuple(int(top[i]*(1-t)+bot[i]*t) for i in range(3))+(255,)
    mask=Image.new("L",(S,S),0)
    ImageDraw.Draw(mask).rounded_rectangle((40,40,S-40,S-40),radius=230,fill=255)
    im=Image.new("RGBA",(S,S),(0,0,0,0)); im.paste(g,(0,0),mask)
    # soft top highlight
    hl=Image.new("L",(S,S),0); ImageDraw.Draw(hl).ellipse((-200,-700,S+200,430),fill=60)
    hl=Image.composite(hl.filter(ImageFilter.GaussianBlur(60)),Image.new("L",(S,S),0),mask)
    im=Image.composite(Image.new("RGBA",(S,S),(255,255,255,255)),im,hl.point(lambda v:v)) if False else im
    white=Image.new("RGBA",(S,S),(255,255,255,0)); white.putalpha(hl); im=Image.alpha_composite(im,white)
    # V chevron (stroked polyline with round joins), plus shadow
    pts=[(300,315),(512,735),(724,315)]
    def vlayer(color,off=(0,0),w=128):
        l=Image.new("RGBA",(S,S),(0,0,0,0)); d=ImageDraw.Draw(l)
        p=[(x+off[0],y+off[1]) for x,y in pts]
        d.line(p,fill=color,width=w,joint="curve")
        for c in (p[0],p[2]): d.ellipse((c[0]-w/2,c[1]-w/2,c[0]+w/2,c[1]+w/2),fill=color)
        return l
    sh=vlayer((30,10,90,150),(0,22)).filter(ImageFilter.GaussianBlur(22))
    im=Image.alpha_composite(im,sh)
    im=Image.alpha_composite(im,vlayer((255,255,255,255)))
    return im
m=master()
m.save(out+".png")
sizes=[(s,s) for s in (16,20,24,32,40,48,64,96,128,256)]
m.save(out+".ico",format="ICO",sizes=sizes)
# small-size preview sheet
sheet=Image.new("RGBA",(16+24+32+48+64+256+80,256),(240,240,240,255)); x=5
for s in (16,24,32,48,64,256):
    sheet.paste(m.resize((s,s),Image.LANCZOS),(x,256-s),m.resize((s,s),Image.LANCZOS)); x+=s+12
sheet.save(out+"_preview.png")
