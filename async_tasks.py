# This program is free software; you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation; either version 3 of the License, or
# (at your option) any later version.
#
# This program is distributed in the hope that it will be useful, but
# WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTIBILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
# General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program. If not, see <http://www.gnu.org/licenses/>.

import bpy
import asyncio
import aiohttp, ssl, certifi

# that's where magic happens; copied from Blender Cloud plugin
from . import async_loop

async def asyncDownloadFile(url, filename: str):
    chunk_size = 100
    ssl_context = ssl.create_default_context(cafile=certifi.where())
    conn = aiohttp.TCPConnector(ssl=ssl_context)

    async with aiohttp.ClientSession(connector=conn) as session:
        async with session.get(url) as response:
            print("starting download")
            length = int(response.headers.get('Content-Length'))
            downloaded = 0
            progress = 0
            print("file length:", length)

            with open(filename, 'wb') as file:
                async for chunk in response.content.iter_chunked(chunk_size):
                    file.write(chunk)
                    downloaded = downloaded + chunk_size
                    currentProgress = int(downloaded/length*100)
                    if currentProgress != progress:
                        progress = currentProgress
                        print("progress:", progress , "%")

    print("DOWNLOAD FINISHED")


def done_callback(context):
    print("TASK HAS FINISHED!!! This is callback function BTW....")
    print("passed argument:", context)


class Test_OT_NoBlock(bpy.types.Operator):
    bl_idname = "view3d.sample_4"
    bl_label = "Asynchronous non-blocking operator as suggested in blender_cloud readme"
    bl_description = "Center 3d cursor after 3s"

    async def act(self):
        ssl_context = ssl.create_default_context(cafile=certifi.where())
        conn = aiohttp.TCPConnector(ssl=ssl_context)
        
        async with aiohttp.ClientSession(connector=conn) as session:
            async with session.get('https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.669577a9736a-linux.x86_64-release.tar.xz') as response:
                print("Download started")
                print("Status:", response.status)
                print("Content-type:", response.headers['content-type'])
                file = await response.read()
                print("Downloaded:", file)

        await asyncio.sleep(3)
        bpy.ops.view3D.snap_cursor_to_center()

    def execute(self, context):
        #async_task = asyncio.ensure_future(self.act())
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender1.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender2.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender3.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender4.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender5.tar.xz"))

        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender6.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender7.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender8.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender9.tar.xz"))
        async_task = asyncio.ensure_future(asyncDownloadFile("https://builder.blender.org/download/daily/blender-3.0.1-candidate+v30.9d6680e7f9b7-linux.x86_64-release.tar.xz", "Blender10.tar.xz"))

        ## It's also possible to handle the task when it's done like so:
        async_task.add_done_callback(done_callback)
        async_loop.ensure_async_loop()
        print("NON-BLOCKING download started")

        return {'FINISHED'}

if __name__ != "__main__":
    print("module", __name__, "imported")

    